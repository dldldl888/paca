package e2e_test

import (
	"context"
	"encoding/json"
	"github.com/Paca-AI/agent-runner/internal/acpbridge"
	"github.com/Paca-AI/agent-runner/internal/messaging"
	"github.com/Paca-AI/agent-runner/internal/repository/postgres"
	"github.com/Paca-AI/agent-runner/internal/runauth"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
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

func TestAuthorizationIngestionDrainsBeforeCommitAndPreservesLawfulTurn(t *testing.T) {
	env := newE2EEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	aid, a := seedACPAgentAndConversation(t, env)
	b := uuid.New()
	sid := uuid.New()
	instance := uuid.NewString()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO agent_chat_sessions(id,agent_id,project_id,member_id)VALUES($1,$2,$3,$4)`, sid, aid, env.projectID, env.memberID)
	exec(`UPDATE agent_conversations SET status='running',chat_session_id=$1 WHERE id=$2`, sid, a)
	exec(`INSERT INTO agent_conversations(id,agent_id,project_id,trigger_type,status,chat_session_id)VALUES($1,$2,$3,'chat_message','running',$4)`, b, aid, env.projectID, sid)
	repo := postgres.NewConversationRepository(env.db)
	var epoch atomic.Value
	epoch.Store("1")
	var transitionRetries atomic.Int32
	var hold atomic.Bool
	hold.Store(true)
	computed := make(chan struct{})
	release := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Token") != "internal" {
			http.Error(w, "unauthorized", 401)
			return
		}
		var body struct {
			ConversationID string `json:"conversation_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		cid, e := uuid.Parse(body.ConversationID)
		if e != nil {
			http.Error(w, "invalid", 400)
			return
		}
		if cid == b && epoch.Load().(string) == "2" && transitionRetries.Add(1) <= 2 {
			http.Error(w, "API gate is still opening", 409)
			return
		}
		status, e := repo.GetStatus(r.Context(), cid)
		if e != nil {
			http.Error(w, "not found", 404)
			return
		}
		if status == "failed" {
			http.Error(w, "revoked", 404)
			return
		}
		data, _ := json.Marshal(map[string]any{"success": true, "data": runauth.Authorization{AuthorizationEpoch: epoch.Load().(string), ConversationID: cid.String(), AgentID: aid.String(), InstanceID: instance, ProjectID: env.projectID.String(), Audience: "owner_private", OwnerUserID: env.userID.String(), Status: status, ChatSessionID: sid.String()}})
		if hold.Swap(false) {
			close(computed)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = w.Write(data)
	}))
	defer api.Close()
	auth := runauth.New(api.URL, "internal", instance)
	auth.Gate = runauth.NewGate()
	_ = auth.Gate.Update("1", false)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	publisher := messaging.NewPublisher(env.redisClient)
	registry := acpbridge.New(env.redisClient, publisher, log)
	server := httptest.NewServer((&acpbridge.Server{Registry: registry, AgentRepo: postgres.NewAgentRepository(env.db), ConvRepo: repo, Publisher: publisher, RunAuthorizer: auth, Log: log}).Routes())
	defer server.Close()
	daemon, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/agent-bridge/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.CloseNow()
	send := func(msg map[string]any) {
		t.Helper()
		if err := wsjson.Write(ctx, daemon, msg); err != nil {
			t.Fatal(err)
		}
	}
	send(map[string]any{"type": "hello", "agent_id": aid.String(), "token": acpDispatchBridgeToken, "boot_id": uuid.NewString()})
	var ack map[string]any
	if err = wsjson.Read(ctx, daemon, &ack); err != nil {
		t.Fatal(err)
	}
	event := func(cid uuid.UUID, marker string) map[string]any {
		return map[string]any{"type": "event", "conversation_id": cid.String(), "authorization_epoch": "1", "event_type": "agent_message_chunk", "payload": map[string]string{"text": marker}}
	}
	send(event(a, "BEFORE_COMMIT"))
	<-computed
	closed := make(chan error, 1)
	go func() { closed <- auth.Gate.Close("2") }()
	for !auth.Gate.State().Blocked {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-closed:
		t.Fatal("CLOSE acknowledged over an admitted protected write")
	default:
	}
	close(release)
	if err = <-closed; err != nil {
		t.Fatal(err)
	}
	if err = waitForEventCount(ctx, env.db, a, 1); err != nil {
		t.Fatal(err)
	}
	// Both messages arrive while closed. The independent reader must still pong;
	// B's original turn must retain its event and terminal after reauthorization.
	send(event(a, "REVOKED_SECRET"))
	send(event(b, "LAWFUL_ORIGINAL_TURN"))
	send(map[string]any{"type": "turn_status", "conversation_id": a.String(), "authorization_epoch": "1", "status": "finished"})
	send(map[string]any{"type": "turn_status", "conversation_id": b.String(), "authorization_epoch": "1", "status": "finished"})
	send(map[string]any{"type": "ping"})
	if err = wsjson.Read(ctx, daemon, &ack); err != nil || ack["type"] != "pong" {
		t.Fatal("control reader blocked by content", ack, err)
	}
	exec(`UPDATE agent_conversations SET status='failed',error_message='Authorization revoked' WHERE id=$1`, a)
	epoch.Store("2")
	_ = auth.Gate.Update("2", false)
	if err = waitForStatus(ctx, env.db, b, "finished"); err != nil {
		t.Fatal("lawful original turn lost terminal", err)
	}
	if transitionRetries.Load() < 3 {
		t.Fatal("B original payload did not retry transitional API 409")
	}
	status, err := repo.GetStatus(ctx, a)
	if err != nil || status != "failed" {
		t.Fatal("late finished resurrected revoked run", status, err)
	}
	var rows []struct {
		ConversationID uuid.UUID `db:"conversation_id"`
		Payload        []byte    `db:"payload"`
	}
	if err = env.db.Select(&rows, `SELECT conversation_id,payload FROM agent_conversation_events WHERE conversation_id IN($1,$2)`, a, b); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want precommit A + same-turn B once; rows=%d", len(rows))
	}
	for _, row := range rows {
		if strings.Contains(string(row.Payload), "REVOKED_SECRET") {
			t.Fatal("revoked late payload persisted")
		}
	}
	stream, err := env.redisClient.XRange(ctx, messaging.StreamAgentEvents, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range stream {
		for _, v := range entry.Values {
			if strings.Contains(v.(string), "REVOKED_SECRET") {
				t.Fatal("revoked late payload streamed")
			}
		}
	}
}
