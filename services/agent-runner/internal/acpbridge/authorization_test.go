package acpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Paca-AI/agent-runner/internal/agent"
	"github.com/Paca-AI/agent-runner/internal/runauth"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNativeAuthorizationACKUsesCurrentSocketAndFencedShutdown(t *testing.T) {
	r, redis := newTestRegistry(t)
	instance := uuid.NewString()
	aid := uuid.New()
	deleting := make(chan string, 3)
	release := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Internal-Token") != "internal-test" {
			http.Error(w, "unauthorized", 401)
			return
		}
		deleting <- req.URL.Path
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]bool{"removed": true}})
	}))
	defer api.Close()
	defer close(release)
	r.WithAuthorization(runauth.New(api.URL, "internal-test", instance), nil)
	sub := redis.Subscribe(t.Context(), "paca:"+instance+":authorization:acks")
	defer sub.Close()
	if _, err := sub.Receive(t.Context()); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		boot, session := uuid.NewString(), uuid.NewString()
		conn := newFakeConn()
		r.mu.Lock()
		r.connections[aid] = &connEntry{bootID: boot, sessionID: session, enrolled: true, conn: conn}
		r.deliveryPhase = "close"
		r.deliveryEpoch = "2"
		r.mu.Unlock()
		if err := r.NativeAck(t.Context(), aid, "stale-socket", "close", "2"); !errors.Is(err, runauth.ErrUnavailable) {
			t.Fatal("stale socket ACK accepted", err)
		}
		if err := r.NativeShutdown(t.Context(), aid, "stale-socket"); !errors.Is(err, runauth.ErrUnavailable) {
			t.Fatal("stale socket unregistered new boot", err)
		}
		done := make(chan error, 1)
		go func() { done <- r.NativeShutdown(t.Context(), aid, session) }()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		msg, err := sub.ReceiveMessage(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		var ack map[string]string
		if err = json.Unmarshal([]byte(msg.Payload), &ack); err != nil {
			t.Fatal(err)
		}
		if ack["boot_id"] != boot || ack["type"] != "closed" {
			t.Fatal("shutdown did not ACK actual boot", ack)
		}
		select {
		case path := <-deleting:
			if path != "/api/internal/authorization-delivery/participants/"+boot {
				t.Fatal(path)
			}
		case <-time.After(time.Second):
			t.Fatal("no unregister after native fence")
		}
		// Deletion may wait for the coordinator lock; its next phase must still ACK
		// from the already-observed native shutdown, without writing to the dead socket.
		if err = r.AuthorizationControl(t.Context(), "reconcile", "2"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel = context.WithTimeout(t.Context(), time.Second)
		msg, err = sub.ReceiveMessage(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal([]byte(msg.Payload), &ack)
		if ack["type"] != "reconciled" || ack["boot_id"] != boot {
			t.Fatal(ack)
		}
		if len(conn.messagesSent()) != 0 {
			t.Fatal("sent native request after shutdown fence")
		}
		release <- struct{}{}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompanyContextDispatchSelectsExplicitHermes(t *testing.T) {
	for _, mode := range []string{"valid", "other_custom", "handle_prefix", "other_provider"} {
		t.Run(mode, func(t *testing.T) {
			registry, redis := newTestRegistry(t)
			aid, cid := uuid.New(), uuid.New()
			instance, owner := uuid.NewString(), uuid.NewString()
			var contextRead bool
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Internal-Token") != "synthetic" {
					t.Error("missing token")
				}
				data := map[string]any{"authorization_epoch": "1", "conversation_id": cid.String(), "agent_id": aid.String(), "instance_id": instance, "owner_user_id": owner}
				switch r.URL.Path {
				case "/api/internal/agent-runs/authorize":
					var in struct {
						Begin bool `json:"begin_turn"`
					}
					json.NewDecoder(r.Body).Decode(&in)
					if mode == "valid" && in.Begin && !contextRead {
						t.Error("native history latch began before context fetch succeeded")
					}
					data["project_id"] = ""
					data["audience"] = "owner_private"
					data["has_prior_turns"] = false
					data["status"] = "running"
					data["task_id"] = ""
					data["comment_id"] = ""
					data["chat_session_id"] = ""
				case "/api/internal/agent-runs/company-context":
					if mode != "valid" {
						t.Error("unrelated ACP configuration read company context")
					}
					contextRead = mode == "valid"
					data["text"] = "合成验证资料 更新时间2026-09-17"
					data["sources"] = []any{}
					data["truncated"] = false
				default:
					t.Error("wrong path")
				}
				json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
			}))
			defer api.Close()
			auth := runauth.New(api.URL, "synthetic", instance)
			auth.Gate = runauth.NewGate()
			if err := auth.Gate.Update("1", false); err != nil {
				t.Fatal(err)
			}
			// Subscribe to the actual dispatch channel: a failed read must not publish.
			sub := redis.Subscribe(t.Context(), dispatchPrefix+aid.String())
			defer sub.Close()
			if _, err := sub.Receive(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := redis.Set(t.Context(), presencePrefix+aid.String(), "online", time.Minute).Err(); err != nil {
				t.Fatal(err)
			}
			dispatcher := &Dispatcher{RunAuthorizer: auth, Registry: registry}
			cfg := &agent.ACPConfig{ID: aid, Handle: "hermes", ACPProvider: "custom"}
			if mode == "other_custom" {
				cfg.Handle = "other"
			}
			if mode == "handle_prefix" {
				cfg.Handle = "hermes-other"
			}
			if mode == "other_provider" {
				cfg.ACPProvider = "claude-code"
			}
			err := dispatcher.DispatchTrigger(t.Context(), agent.Trigger{ConversationID: cid, Message: "公司事实?"}, cfg)

			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			msg, err := sub.ReceiveMessage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(msg.Payload), &payload); err != nil {
				t.Fatal(err)
			}
			if mode == "valid" {
				if payload["display_message"] != "公司事实?" {
					t.Fatalf("missing original display input: %v", payload["display_message"])
				}
			} else if _, exists := payload["display_message"]; exists {
				t.Fatal("changed unrelated ACP display")
			}
			if mode != "valid" {
				if strings.Contains(msg.Payload, "合成验证资料") || !strings.Contains(msg.Payload, "/paca") {
					t.Fatal("changed unrelated ACP prompt", msg.Payload)
				}
				return
			}
			if !strings.Contains(msg.Payload, "合成验证资料") || strings.Contains(msg.Payload, "/paca") || strings.Contains(msg.Payload, "synthetic") {
				t.Fatal(msg.Payload)
			}
		})
	}
}
