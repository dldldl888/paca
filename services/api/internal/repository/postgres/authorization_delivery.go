package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/database"
	"github.com/Paca-AI/api/internal/platform/delivery"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
)

// DeliveryQuery reuses the authority transaction for existing policy and object readers.
func DeliveryQuery(ctx context.Context, db *sqlx.DB) deliveryQuery {
	if tx := database.Transaction(ctx); tx != nil {
		return tx
	}
	return db
}

type deliveryQuery interface {
	sqlx.ExtContext
	GetContext(context.Context, any, string, ...any) error
	SelectContext(context.Context, any, string, ...any) error
	NamedExecContext(context.Context, string, any) (sql.Result, error)
}

type DeliveryState struct {
	Epoch   string `json:"epoch" db:"epoch"`
	Blocked bool   `json:"blocked" db:"blocked"`
}
type deliveryControl struct {
	Type   string `json:"type"`
	Epoch  string `json:"epoch"`
	BootID string `json:"boot_id,omitempty"`
}

var ErrDeliveryBlocked = errors.New("authorization transition pending")

// ponytail: global delivery pauses during authority changes; partition only after measured contention.
type AuthorizationDelivery struct {
	db        *sqlx.DB
	redis     *redis.Client
	instance  string
	Reconcile func(context.Context) error
}

var deliveryCoordinators sync.Map // one configured coordinator per existing database handle
func NewAuthorizationDelivery(db *sqlx.DB, client *redis.Client, instance string) *AuthorizationDelivery {
	return &AuthorizationDelivery{db: db, redis: client, instance: instance}
}
func (d *AuthorizationDelivery) Install() { deliveryCoordinators.Store(d.db, d) }
func (d *AuthorizationDelivery) ControlChannel() string {
	return "paca:" + d.instance + ":authorization:control"
}
func (d *AuthorizationDelivery) AckChannel() string {
	return "paca:" + d.instance + ":authorization:acks"
}
func (d *AuthorizationDelivery) State(ctx context.Context) (DeliveryState, error) {
	var s DeliveryState
	err := DeliveryQuery(ctx, d.db).GetContext(ctx, &s, `SELECT epoch::text,blocked FROM authorization_delivery_state WHERE instance_id=$1`, d.instance)
	return s, err
}
func (d *AuthorizationDelivery) lock(ctx context.Context) (*sql.Conn, error) {
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	// Session lock survives transaction boundaries and cannot expire underneath a delayed emitter.
	if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock(734228905116)`); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
func releaseDeliveryLock(conn *sql.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_unlock(734228905116)`); err != nil {
		// Never return a possibly locked session to the pool.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	_ = conn.Close()
}
func (d *AuthorizationDelivery) Enroll(ctx context.Context, boot, kind string) (DeliveryState, error) {
	if id, err := uuid.Parse(boot); err != nil || id == uuid.Nil {
		return DeliveryState{}, errors.New("invalid boot id")
	}
	if kind != "api" && kind != "runner" && kind != "realtime" {
		return DeliveryState{}, errors.New("invalid participant kind")
	}
	conn, err := d.lock(ctx)
	if err != nil {
		return DeliveryState{}, err
	}
	defer releaseDeliveryLock(conn)
	if _, err = conn.ExecContext(ctx, `INSERT INTO authorization_delivery_state(instance_id) VALUES($1) ON CONFLICT DO NOTHING`, d.instance); err != nil {
		return DeliveryState{}, err
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO authorization_delivery_participants(instance_id,boot_id,kind) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, d.instance, boot, kind); err != nil {
		return DeliveryState{}, err
	}
	return d.State(ctx)
}
func (d *AuthorizationDelivery) Unenroll(ctx context.Context, boot string) error {
	conn, err := d.lock(ctx)
	if err != nil {
		return err
	}
	defer releaseDeliveryLock(conn)
	_, err = conn.ExecContext(ctx, `DELETE FROM authorization_delivery_participants WHERE instance_id=$1 AND boot_id=$2`, d.instance, boot)
	return err
}
func authorityMutation(ctx context.Context, db *sqlx.DB, fn func(context.Context) error) error {
	if database.Transaction(ctx) != nil {
		return fn(ctx)
	}
	if value, ok := deliveryCoordinators.Load(db); ok {
		return value.(*AuthorizationDelivery).Mutate(ctx, fn)
	}
	return fn(ctx)
}
func (d *AuthorizationDelivery) Mutate(ctx context.Context, fn func(context.Context) error) error {
	conn, err := d.lock(ctx)
	if err != nil {
		return err
	}
	defer releaseDeliveryLock(conn)
	if _, err = conn.ExecContext(ctx, `INSERT INTO authorization_delivery_state(instance_id) VALUES($1) ON CONFLICT DO NOTHING`, d.instance); err != nil {
		return err
	}
	sub := d.redis.Subscribe(ctx, d.AckChannel())
	defer sub.Close()
	if _, err = sub.Receive(ctx); err != nil {
		return err
	}
	var epoch string
	if err = conn.QueryRowContext(ctx, `UPDATE authorization_delivery_state SET epoch=epoch+1,blocked=true WHERE instance_id=$1 RETURNING epoch::text`, d.instance).Scan(&epoch); err != nil {
		return err
	}
	var participants []string
	if err = d.db.SelectContext(ctx, &participants, `SELECT boot_id::text FROM authorization_delivery_participants WHERE instance_id=$1`, d.instance); err != nil {
		return err
	}
	if err = d.barrier(ctx, sub, participants, "close", "closed", epoch); err != nil {
		return err
	}
	// A crashed prior coordinator may already have committed. Reconciliation always uses current DB truth.
	err = WithTx(ctx, d.db, func(tx *sqlx.Tx) error {
		txctx := database.WithTransaction(ctx, tx)
		if fn != nil {
			if err := authz.RecheckMutation(txctx); err != nil {
				return err
			}
		}
		if fn != nil {
			if err := fn(txctx); err != nil {
				return err
			}
		}
		if d.Reconcile != nil {
			return d.Reconcile(txctx)
		}
		return nil
	})
	// Any error/ambiguous COMMIT leaves the durable gate closed. A later recovery retries from DB truth.
	if err != nil {
		return err
	}
	if err = d.barrier(ctx, sub, participants, "reconcile", "reconciled", epoch); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `UPDATE authorization_delivery_state SET blocked=false WHERE instance_id=$1 AND epoch=$2`, d.instance, epoch); err != nil {
		return err
	}
	return d.publish(ctx, "open", epoch)
}
func (d *AuthorizationDelivery) publish(ctx context.Context, kind, epoch string) error {
	body, _ := json.Marshal(deliveryControl{Type: kind, Epoch: epoch})
	return d.redis.Publish(ctx, d.ControlChannel(), body).Err()
}
func (d *AuthorizationDelivery) barrier(ctx context.Context, sub *redis.PubSub, participants []string, kind, ack, epoch string) error {
	expected := make(map[string]bool, len(participants))
	for _, id := range participants {
		expected[id] = true
	}
	if err := d.publish(ctx, kind, epoch); err != nil {
		return err
	}
	for len(expected) > 0 {
		var msg *redis.Message
		select {
		case <-ctx.Done():
			return fmt.Errorf("authorization %s ACK: %w", ack, ctx.Err())
		case msg = <-sub.Channel():
			if msg == nil {
				return errors.New("authorization ACK subscription closed")
			}
		}
		var control deliveryControl
		if json.Unmarshal([]byte(msg.Payload), &control) == nil && control.Type == ack && control.Epoch == epoch {
			delete(expected, control.BootID)
		}
	}
	return nil
}

// StartParticipant subscribes before durable enrollment. The boot ID is never reused.
func (d *AuthorizationDelivery) StartParticipant(ctx context.Context, gate *delivery.Gate) (func(context.Context) error, error) {
	boot := uuid.NewString()
	sub := d.redis.Subscribe(ctx, d.ControlChannel())
	if _, err := sub.Receive(ctx); err != nil {
		sub.Close()
		return nil, err
	}
	runctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer gate.Disconnect()
		for {
			msg, err := sub.ReceiveMessage(runctx)
			if err != nil {
				gate.Disconnect()
				if runctx.Err() != nil {
					return
				}
				continue
			}
			var c deliveryControl
			if json.Unmarshal([]byte(msg.Payload), &c) != nil {
				continue
			}
			kind := ""
			switch c.Type {
			case "close":
				if gate.Close(c.Epoch) != nil {
					continue
				}
				kind = "closed"
			case "reconcile":
				if gate.State().Epoch != c.Epoch || !gate.State().Blocked {
					continue
				}
				kind = "reconciled"
			case "open":
				state, err := d.State(runctx)
				if err == nil && !state.Blocked && state.Epoch == c.Epoch {
					_ = gate.Update(c.Epoch, false)
				}
			}
			if kind != "" {
				body, _ := json.Marshal(deliveryControl{Type: kind, Epoch: c.Epoch, BootID: boot})
				if d.redis.Publish(runctx, d.AckChannel(), body).Err() != nil {
					gate.Disconnect()
				}
			}
		}
	}()
	state, err := d.Enroll(ctx, boot, "api")
	if err != nil {
		cancel()
		sub.Close()
		return nil, err
	}
	if err = gate.Update(state.Epoch, state.Blocked); err != nil {
		cancel()
		sub.Close()
		return nil, err
	}
	return func(ctx context.Context) error {
		gate.Disconnect()
		cancel()
		sub.Close()
		return d.Unenroll(ctx, boot)
	}, nil
}
