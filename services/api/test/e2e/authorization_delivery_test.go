package e2e_test

import (
	"context"
	"encoding/json"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestAuthorizationDeliveryCommitWaitsForDurableACK(t *testing.T) {
	env := newE2EEnv(t)
	instance := uuid.NewString()
	d := pg.NewAuthorizationDelivery(env.db, env.redisClient, instance)
	boot := uuid.NewString()
	if _, err := d.Enroll(env.ctx, boot, "realtime"); err != nil {
		t.Fatal(err)
	}
	sub := env.redisClient.Subscribe(env.ctx, d.ControlChannel())
	defer sub.Close()
	if _, err := sub.Receive(env.ctx); err != nil {
		t.Fatal(err)
	}
	messages := sub.Channel()
	if _, err := env.db.Exec(`CREATE TABLE delivery_probe (id integer primary key, value integer); INSERT INTO delivery_probe VALUES(1,0)`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- d.Mutate(env.ctx, func(ctx context.Context) error {
			_, err := pg.DeliveryQuery(ctx, env.db).ExecContext(ctx, `UPDATE delivery_probe SET value=1 WHERE id=1`)
			return err
		})
	}()
	ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
	defer cancel()
	var control struct{ Type, Epoch string }
	select {
	case msg := <-messages:
		if err := json.Unmarshal([]byte(msg.Payload), &control); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if control.Type != "close" {
		t.Fatalf("first control=%s", control.Type)
	}
	var value int
	if err := env.db.Get(&value, `SELECT value FROM delivery_probe WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if value != 0 {
		t.Fatal("authority committed without CLOSE ACK")
	}
	ack := func(kind string) {
		body, _ := json.Marshal(map[string]string{"type": kind, "epoch": control.Epoch, "boot_id": boot})
		if err := env.redisClient.Publish(ctx, d.AckChannel(), body).Err(); err != nil {
			t.Fatal(err)
		}
	}
	ack("closed")
	select {
	case msg := <-messages:
		if err := json.Unmarshal([]byte(msg.Payload), &control); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if control.Type != "reconcile" {
		t.Fatalf("second control=%s", control.Type)
	}
	if err := env.db.Get(&value, `SELECT value FROM delivery_probe WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if value != 1 {
		t.Fatal("reconcile preceded commit")
	}
	state, err := d.State(ctx)
	if err != nil || !state.Blocked {
		t.Fatalf("state before native ACK: %#v %v", state, err)
	}
	ack("reconciled")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	state, err = d.State(ctx)
	if err != nil || state.Blocked {
		t.Fatalf("state after ACK: %#v %v", state, err)
	}
	// A coordinator restart retains the enrollment even after Redis forgets all keys.
	if err := env.redisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	restarted := pg.NewAuthorizationDelivery(env.db, env.redisClient, instance)
	timeout, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	err = restarted.Mutate(timeout, func(ctx context.Context) error {
		_, e := pg.DeliveryQuery(ctx, env.db).ExecContext(ctx, `UPDATE delivery_probe SET value=2`)
		return e
	})
	if err == nil {
		t.Fatal("missing durable participant ACK was ignored")
	}
	if err := env.db.Get(&value, `SELECT value FROM delivery_probe`); err != nil || value != 1 {
		t.Fatalf("missing ACK mutated authority: %d %v", value, err)
	}
	state, err = d.State(ctx)
	if err != nil || !state.Blocked {
		t.Fatalf("failed coordinator reopened: %#v %v", state, err)
	}
}
