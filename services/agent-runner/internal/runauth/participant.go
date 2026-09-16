package runauth

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"net/http"
	"time"
)

func (c *Client) internal(ctx context.Context, path, method string, body any, out any) error {
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, method, c.Origin+"/api/internal/authorization-delivery/"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("X-Internal-Token", c.InternalKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ErrUnavailable
	}
	var e struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&e) != nil || !e.Success {
		return ErrUnavailable
	}
	if out != nil {
		return json.Unmarshal(e.Data, out)
	}
	return nil
}
func (c *Client) Enroll(ctx context.Context, boot string) (State, error) {
	var state State
	err := c.internal(ctx, "participants", "POST", map[string]string{"boot_id": boot, "kind": "runner"}, &state)
	return state, err
}
func (c *Client) Unenroll(ctx context.Context, boot string) error {
	return c.internal(ctx, "participants/"+boot, "DELETE", nil, nil)
}
func (c *Client) DeliveryState(ctx context.Context) (State, error) {
	var state State
	err := c.internal(ctx, "state", "GET", nil, &state)
	return state, err
}
func (c *Client) Ack(ctx context.Context, redisClient *redis.Client, boot, phase, epoch string) error {
	kind := "closed"
	if phase == "reconcile" {
		kind = "reconciled"
	}
	if phase == "open" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"type": kind, "epoch": epoch, "boot_id": boot})
	return redisClient.Publish(ctx, "paca:"+c.InstanceID+":authorization:acks", body).Err()
}
func (c *Client) StartParticipant(ctx context.Context, redisClient *redis.Client, control func(context.Context, string, string) error) (func(context.Context) error, error) {
	c.Gate = NewGate()
	boot := uuid.NewString()
	channel := "paca:" + c.InstanceID + ":authorization:control"
	sub := redisClient.Subscribe(ctx, channel)
	if _, err := sub.Receive(ctx); err != nil {
		sub.Close()
		return nil, err
	}
	runctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer c.Gate.Disconnect()
		for {
			msg, err := sub.ReceiveMessage(runctx)
			if err != nil {
				c.Gate.Disconnect()
				if runctx.Err() != nil {
					return
				}
				continue
			}
			var m struct{ Type, Epoch string }
			if json.Unmarshal([]byte(msg.Payload), &m) != nil {
				continue
			}
			phaseCtx, stop := context.WithTimeout(runctx, 25*time.Second)
			switch m.Type {
			case "close":
				err = c.Gate.Close(m.Epoch)
			case "reconcile":
				if c.Gate.State().Epoch != m.Epoch || !c.Gate.State().Blocked {
					stop()
					continue
				}
			case "open":
				var s State
				s, err = c.DeliveryState(phaseCtx)
				if err == nil && (s.Blocked || s.Epoch != m.Epoch) {
					err = ErrUnavailable
				}
			default:
				stop()
				continue
			}
			if err == nil {
				err = control(phaseCtx, m.Type, m.Epoch)
			}
			if err == nil && m.Type == "open" {
				err = c.Gate.Update(m.Epoch, false)
			}
			if err == nil {
				err = c.Ack(phaseCtx, redisClient, boot, m.Type, m.Epoch)
			}
			if err != nil {
				c.Gate.Disconnect()
			}
			stop()
		}
	}()
	state, err := c.Enroll(ctx, boot)
	if err != nil {
		cancel()
		sub.Close()
		return nil, err
	}
	// Native controls must complete before the local admission gate opens.
	if !state.Blocked {
		if err = control(ctx, "open", state.Epoch); err != nil {
			c.Gate.Disconnect()
			cancel()
			sub.Close()
			return nil, err
		}
	}
	if err = c.Gate.Update(state.Epoch, state.Blocked); err != nil {
		cancel()
		sub.Close()
		return nil, err
	}
	return func(ctx context.Context) error {
		c.Gate.Disconnect()
		cancel()
		sub.Close()
		return c.Unenroll(ctx, boot)
	}, nil
}
func (c *Client) Enter(ctx context.Context) (context.Context, string, func(), error) {
	if c == nil || c.Gate == nil {
		return ctx, "", func() {}, nil
	}
	return c.Gate.Enter(ctx)
}
