package e2e_test

import (
	"context"
	"encoding/json"
	"github.com/Paca-AI/agent-runner/internal/acpbridge"
	"github.com/Paca-AI/agent-runner/internal/agent"
	"github.com/Paca-AI/agent-runner/internal/messaging"
	"github.com/Paca-AI/agent-runner/internal/repository/postgres"
	"github.com/Paca-AI/agent-runner/internal/runauth"
	"github.com/google/uuid"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompanyContextFailureIsVisibleAndNeverDispatched(t *testing.T) {
	// TestMain's first PostgreSQL ready log can precede the externally reachable
	// server. Wait for its SQL connection before the helper opens its admin DB.
	if sharedPGDSN != "" {
		deadline := time.Now().Add(5 * time.Second)
		for {
			probe, err := postgres.Open(sharedPGDSN)
			if err == nil {
				_ = probe.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	env := newE2EEnv(t)
	aid, cid := seedACPAgentAndConversation(t, env)
	instance := uuid.NewString()
	// The real API accepts custom, not a fictional "hermes" provider.
	if _, err := env.db.Exec(`UPDATE agents SET handle='hermes',acp_provider='custom',acp_command='["python","launch_hermes.py"]' WHERE id=$1`, aid); err != nil {
		t.Fatal(err)
	}
	cfg, err := postgres.NewAgentRepository(env.db).FindACPByID(t.Context(), aid)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "read_failure", "epoch_mismatch", "blocked", "begin_failure", "begin_identity_mismatch", "begin_epoch_mismatch", "begin_blocked", "already_stopped"} {
		t.Run(mode, func(t *testing.T) {
			if _, err := env.db.Exec(`UPDATE agent_conversations SET status='running',error_message=NULL WHERE id=$1`, cid); err != nil {
				t.Fatal(err)
			}
			var contextRead atomic.Bool
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/internal/agent-runs/company-context" {
					if mode == "blocked" {
						w.WriteHeader(409)
						return
					}
					if mode == "read_failure" {
						w.WriteHeader(500)
						return
					}
					contextRead.Store(true)
					epoch := "1"
					if mode == "epoch_mismatch" {
						epoch = "2"
					}
					if mode == "already_stopped" {
						if _, e := env.db.Exec(`UPDATE agent_conversations SET status='stopped',error_message='existing terminal' WHERE id=$1`, cid); e != nil {
							t.Error(e)
						}
						w.WriteHeader(500)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"authorization_epoch": epoch, "conversation_id": cid.String(), "agent_id": aid.String(), "instance_id": instance, "owner_user_id": env.userID.String(), "text": "合成公司资料 更新时间2026-09-17", "sources": []any{}, "truncated": false}})
					return
				}
				var body struct {
					Begin bool `json:"begin_turn"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				if body.Begin && !contextRead.Load() {
					t.Error("began before company context")
				}
				if body.Begin && mode == "begin_failure" {
					w.WriteHeader(500)
					return
				}
				if body.Begin && mode == "begin_blocked" {
					w.WriteHeader(409)
					return
				}
				epoch := "1"
				if body.Begin && mode == "begin_epoch_mismatch" {
					epoch = "2"
				}
				owner := env.userID.String()
				if body.Begin && mode == "begin_identity_mismatch" {
					owner = uuid.NewString()
				}
				json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"authorization_epoch": epoch, "conversation_id": cid.String(), "agent_id": aid.String(), "instance_id": instance, "owner_user_id": owner, "project_id": env.projectID.String(), "audience": "owner_private", "has_prior_turns": false, "status": "running", "task_id": "", "comment_id": "", "chat_session_id": ""}})
			}))
			defer api.Close()
			auth := runauth.New(api.URL, "synthetic", instance)
			auth.Gate = runauth.NewGate()
			if err := auth.Gate.Update("1", false); err != nil {
				t.Fatal(err)
			}
			publisher := messaging.NewPublisher(env.redisClient)
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			registry := acpbridge.New(env.redisClient, publisher, log)
			conn := &privateRunConn{messages: make(chan map[string]any, 8)}
			registration, err := registry.Register(t.Context(), aid, &env.projectID, conn)
			if err != nil {
				t.Fatal(err)
			}
			defer registry.Unregister(context.Background(), aid, &env.projectID, registration)
			dispatcher := &acpbridge.Dispatcher{RunAuthorizer: auth, Registry: registry, ConvRepo: postgres.NewConversationRepository(env.db), Publisher: publisher, Log: log}
			before, _ := env.redisClient.XLen(t.Context(), messaging.StreamAgentConversationStatus).Result()
			err = dispatcher.DispatchTrigger(t.Context(), agent.Trigger{ConversationID: cid, AgentID: aid}, cfg)
			if err != nil {
				t.Fatal("handled failure should be acknowledged", err)
			}
			var status string
			var message *string
			if e := env.db.QueryRow(`SELECT status,error_message FROM agent_conversations WHERE id=$1`, cid).Scan(&status, &message); e != nil {
				t.Fatal(e)
			}
			if mode == "valid" {
				if status != "running" {
					t.Fatal(status)
				}
				select {
				case msg := <-conn.messages:
					prompt, _ := msg["message"].(string)
					if !strings.Contains(prompt, "合成公司资料") || !strings.Contains(prompt, "公司助手") || strings.Contains(prompt, "/paca") {
						t.Fatal("real custom configuration bypassed company prompt", prompt)
					}
				case <-time.After(time.Second):
					t.Fatal("no company dispatch")
				}
				return
			}
			if mode == "already_stopped" {
				if status != "stopped" || message == nil || *message != "existing terminal" {
					t.Fatal("overwrote existing terminal", status, message)
				}
			} else {
				if status != "failed" || message == nil || *message != "公司资料暂时无法读取，请稍后重试。" {
					t.Fatal("context failure not visible", status, message)
				}
				after, _ := env.redisClient.XLen(t.Context(), messaging.StreamAgentConversationStatus).Result()
				if after != before+1 {
					t.Fatal("failure status not published")
				}
			}
			select {
			case msg := <-conn.messages:
				t.Fatal("failed context dispatched", msg)
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}
