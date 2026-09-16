package e2e_test

import (
	"context"
	"encoding/json"
	"github.com/Paca-AI/agent-runner/internal/acpbridge"
	"github.com/Paca-AI/agent-runner/internal/agent"
	"github.com/Paca-AI/agent-runner/internal/config"
	"github.com/Paca-AI/agent-runner/internal/handler"
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

type privateRunConn struct{ messages chan map[string]any }

func (c *privateRunConn) SendJSON(_ context.Context, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	var m map[string]any
	if e = json.Unmarshal(b, &m); e != nil {
		return e
	}
	c.messages <- m
	return nil
}
func (c *privateRunConn) Close(int, string) error { return nil }

// A real PG/Valkey dispatch plus an authenticated HTTP contract peer. Current
// permission policy is covered by the API's real-PG matrix, not copied here.
func TestPrivateRunTrustedDispatchAndQueueReauthorization(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	aid, cid := seedACPAgentAndConversation(t, env)
	sid := uuid.New()
	instance := uuid.NewString()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := env.db.Exec(q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO agent_chat_sessions(id,agent_id,project_id,member_id) VALUES($1,$2,$3,$4)`, sid, aid, env.projectID, env.memberID)
	canonicalTask := uuid.NewString()
	refs, _ := json.Marshal([]agent.ContextItemRef{{Type: "task", ID: canonicalTask, Title: "canonical context"}})
	exec(`UPDATE agent_conversations SET chat_session_id=$1,context_items=$2,status='running' WHERE id=$3`, sid, refs, cid)
	var mode atomic.Int32
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent-runs/authorize" || r.Header.Get("X-Internal-Token") != "synthetic-internal" {
			http.Error(w, "unauthorized", 401)
			return
		}
		var body struct {
			ConversationID string `json:"conversation_id"`
			AgentID        string `json:"agent_id"`
			Begin          bool   `json:"begin_turn"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.ConversationID != cid.String() || body.AgentID != aid.String() {
			t.Error("identity not DB-derived")
			http.Error(w, "bad identity", 400)
			return
		}
		calls.Add(1)
		if mode.Load() == 1 || mode.Load() == 2 && body.Begin {
			http.Error(w, "revoked", 404)
			return
		}
		data := map[string]any{"conversation_id": cid.String(), "agent_id": aid.String(), "instance_id": instance, "project_id": env.projectID.String(), "audience": "owner_private", "owner_user_id": env.userID.String(), "has_prior_turns": body.Begin && mode.Load() == 4, "status": "running", "task_id": "", "comment_id": "", "chat_session_id": sid.String()}
		if mode.Load() == 3 {
			delete(data, "owner_user_id")
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
	}))
	defer api.Close()
	auth := runauth.New(api.URL, "synthetic-internal", instance)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	publisher := messaging.NewPublisher(env.redisClient)
	repo := postgres.NewConversationRepository(env.db)
	registry := acpbridge.New(env.redisClient, publisher, log)
	conn := &privateRunConn{messages: make(chan map[string]any, 8)}
	registration, e := registry.Register(ctx, aid, &env.projectID, conn)
	if e != nil {
		t.Fatal(e)
	}
	defer registry.Unregister(context.Background(), aid, &env.projectID, registration)
	dispatcher := &acpbridge.Dispatcher{RunAuthorizer: auth, Registry: registry, ConvRepo: repo, Publisher: publisher, Log: log}
	h := &handler.Handler{RunAuthorizer: auth, Gate: config.NewGate([]string{aid.String()}), AgentRepo: postgres.NewAgentRepository(env.db), ConvRepo: repo, Publisher: publisher, ACPDispatcher: dispatcher, Log: log}
	spoof := uuid.New()
	trigger := agent.Trigger{ConversationID: cid, AgentID: aid, ProjectID: spoof, ActorUserID: &spoof, ActorMemberID: &spoof, TaskID: &spoof, CommentID: &spoof, TriggerType: agent.TriggerDescriptionWrite, ContextItems: []agent.ContextItemRef{{Type: "task", ID: spoof.String(), Title: "queue secret"}}, Message: "hello"}
	t.Run("trusted_identity_and_persisted_context", func(t *testing.T) {
		if e := h.Handle(ctx, trigger); e != nil {
			t.Fatal(e)
		}
		select {
		case msg := <-conn.messages:
			for k, want := range map[string]any{"type": "start_turn", "audience": "owner_private", "owner_user_id": env.userID.String(), "agent_id": aid.String(), "instance_id": instance, "project_id": env.projectID.String(), "conversation_id": cid.String(), "chat_session_id": sid.String(), "has_prior_turns": false, "task_id": "", "comment_id": "", "trigger_type": "chat_message"} {
				if msg[k] != want {
					t.Errorf("%s=%v want %v", k, msg[k], want)
				}
			}
			prompt, _ := msg["message"].(string)
			if !strings.Contains(prompt, canonicalTask) || !strings.Contains(prompt, "canonical context") || strings.Contains(prompt, "queue secret") || strings.Contains(prompt, spoof.String()) || strings.Contains(prompt, "synthetic-internal") {
				t.Fatalf("untrusted context reached prompt: %s", prompt)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no authorized dispatch")
		}
		if calls.Load() != 2 {
			t.Fatalf("want preflight and begin checks, got %d", calls.Load())
		}
	})
	for _, tc := range []struct {
		name string
		mode int32
	}{{"revoked_while_queued", 1}, {"revoked_before_begin", 2}, {"missing_owner", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			mode.Store(tc.mode)
			exec(`UPDATE agent_conversations SET status='running' WHERE id=$1`, cid)
			before, _ := env.redisClient.XLen(ctx, messaging.StreamAgentConversationStatus).Result()
			if e := h.Handle(ctx, trigger); e != nil {
				t.Fatal(e)
			}
			status, e := conversationStatus(ctx, env.db, cid)
			if e != nil || status != "failed" {
				t.Fatalf("denied run not failed: %s %v", status, e)
			}
			after, _ := env.redisClient.XLen(ctx, messaging.StreamAgentConversationStatus).Result()
			if after != before+1 {
				t.Fatal("failed run did not release queue")
			}
			select {
			case msg := <-conn.messages:
				t.Fatalf("denied prompt dispatched: %v", msg)
			case <-time.After(100 * time.Millisecond):
			}
			var events int
			if e := env.db.Get(&events, `SELECT count(*) FROM agent_conversation_events WHERE conversation_id=$1`, cid); e != nil {
				t.Fatal(e)
			}
			if events != 0 {
				t.Fatal("denied run wrote task/prompt events")
			}
		})
	}
	t.Run("fix1_legacy_shared_attachment", func(t *testing.T) {
		h.RunAuthorizer = nil
		dispatcher.RunAuthorizer = nil
		exec(`UPDATE agent_conversations SET chat_session_id=NULL,context_items=NULL,status='running' WHERE id=$1`, cid)
		legacy := agent.Trigger{ConversationID: cid, AgentID: aid, Message: "accepted legacy", ContextItems: []agent.ContextItemRef{{Type: "doc", ID: canonicalTask, Title: "authorized legacy attachment"}}}
		if e := h.Handle(ctx, legacy); e != nil {
			t.Fatal(e)
		}
		select {
		case msg := <-conn.messages:
			prompt, _ := msg["message"].(string)
			if !strings.Contains(prompt, "authorized legacy attachment") || !strings.Contains(prompt, canonicalTask) {
				t.Errorf("legacy attachment dropped: %s", prompt)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("legacy turn missing")
		}
		h.RunAuthorizer = auth
		dispatcher.RunAuthorizer = auth
		exec(`UPDATE agent_conversations SET chat_session_id=$1,context_items=$2 WHERE id=$3`, sid, refs, cid)
	})
	t.Run("wrong_queue_agent_and_malformed_trigger_are_terminal", func(t *testing.T) {
		mode.Store(0)
		for _, bad := range []agent.Trigger{{ConversationID: cid, AgentID: uuid.New()}, {ConversationID: cid, Malformed: true}} {
			exec(`UPDATE agent_conversations SET status='running' WHERE id=$1`, cid)
			before := calls.Load()
			if e := h.Handle(ctx, bad); e != nil {
				t.Fatal(e)
			}
			status, e := conversationStatus(ctx, env.db, cid)
			if e != nil || status != "failed" || calls.Load() != before {
				t.Fatalf("invalid queue routed to API or executor: %s %v", status, e)
			}
		}
	})
}
