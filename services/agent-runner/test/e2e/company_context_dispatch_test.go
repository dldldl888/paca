package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
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
	"testing"
	"time"
)

func TestCompanyContextFailureIsVisibleAndNeverDispatched(t *testing.T) {
	env := newE2EEnv(t)
	aid, cid := seedACPAgentAndConversation(t, env)
	instance := uuid.NewString()
	for _, mode := range []string{"read_failure", "epoch_mismatch", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			if _, err := env.db.Exec(`UPDATE agent_conversations SET status='running',error_message=NULL WHERE id=$1`, cid); err != nil {
				t.Fatal(err)
			}
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
					json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"authorization_epoch": "2", "conversation_id": cid.String(), "agent_id": aid.String(), "instance_id": instance, "owner_user_id": env.userID.String(), "text": "must not disclose", "sources": []any{}, "truncated": false}})
					return
				}
				var body struct {
					Begin bool `json:"begin_turn"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				if body.Begin {
					t.Error("failure began a native turn")
				}
				json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"authorization_epoch": "1", "conversation_id": cid.String(), "agent_id": aid.String(), "instance_id": instance, "owner_user_id": env.userID.String(), "project_id": env.projectID.String(), "audience": "owner_private", "has_prior_turns": false, "status": "running", "task_id": "", "comment_id": "", "chat_session_id": ""}})
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
			err = dispatcher.DispatchTrigger(t.Context(), agent.Trigger{ConversationID: cid, AgentID: aid}, &agent.ACPConfig{ID: aid, ACPProvider: "hermes"})
			if mode == "blocked" {
				if !errors.Is(err, runauth.ErrBlocked) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal("visible failure should be acknowledged", err)
			}
			var status string
			var message *string
			if e := env.db.QueryRow(`SELECT status,error_message FROM agent_conversations WHERE id=$1`, cid).Scan(&status, &message); e != nil {
				t.Fatal(e)
			}
			if mode == "blocked" {
				if status != "running" {
					t.Fatal("barrier terminalized", status)
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
