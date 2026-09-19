package acpbridge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Paca-AI/agent-runner/internal/messaging"
)

func newTestRegistry(t *testing.T) (*Registry, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { client.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(client, messaging.NewPublisher(client), log), client
}

// fakeConn is a Conn test double recording every call, safe for concurrent
// use since Registry's background goroutines call it from outside the test
// body's own goroutine.
type fakeConn struct {
	mu        sync.Mutex
	sent      []any
	closed    bool
	closeCode int
	closeMsg  string
	closeCh   chan struct{}
}

func newFakeConn() *fakeConn {
	return &fakeConn{closeCh: make(chan struct{})}
}

func (f *fakeConn) SendJSON(_ context.Context, v any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, v)
	return nil
}

func (f *fakeConn) Close(code int, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		f.closeCode = code
		f.closeMsg = reason
		close(f.closeCh)
	}
	return nil
}

func (f *fakeConn) wasClosed(t *testing.T, within time.Duration) bool {
	t.Helper()
	select {
	case <-f.closeCh:
		return true
	case <-time.After(within):
		return false
	}
}

func (f *fakeConn) messagesSent() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]any, len(f.sent))
	copy(out, f.sent)
	return out
}

func TestRegister_SetsPresenceAndReturnsASessionID(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	agentID := uuid.New()
	conn := newFakeConn()

	sessionID, err := r.Register(ctx, agentID, nil, conn)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if sessionID == "" {
		t.Error("Register returned an empty sessionID")
	}

	online, err := r.IsOnline(ctx, agentID)
	if err != nil {
		t.Fatalf("IsOnline: %v", err)
	}
	if !online {
		t.Error("IsOnline = false immediately after Register")
	}
}

func TestIsOnline_FalseForAnUnregisteredAgent(t *testing.T) {
	r, _ := newTestRegistry(t)
	online, err := r.IsOnline(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("IsOnline: %v", err)
	}
	if online {
		t.Error("IsOnline = true for an agent that was never registered")
	}
}

func TestUnregister_ClearsPresence(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	agentID := uuid.New()
	conn := newFakeConn()

	sessionID, err := r.Register(ctx, agentID, nil, conn)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	r.Unregister(ctx, agentID, nil, sessionID)

	online, err := r.IsOnline(ctx, agentID)
	if err != nil {
		t.Fatalf("IsOnline: %v", err)
	}
	if online {
		t.Error("IsOnline = true after Unregister")
	}
}

// TestUnregister_ReturnsPromptly pins the real bug found live while
// building this package: PubSub.ReceiveMessage(ctx) does not reliably
// abort on context cancellation alone (see drainMessages's doc comment) —
// Unregister used to be able to hang indefinitely waiting for its
// background goroutines to notice. A generous bound, not a tight one: the
// point is "returns at all in a reasonable time," not exact timing.
func TestUnregister_ReturnsPromptly(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	agentID := uuid.New()
	conn := newFakeConn()

	sessionID, err := r.Register(ctx, agentID, nil, conn)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	done := make(chan struct{})
	go func() {
		r.Unregister(ctx, agentID, nil, sessionID)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Unregister did not return within 3s")
	}
}

func TestUnregister_StaleSessionIDIsANoOp(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	agentID := uuid.New()
	conn := newFakeConn()

	if _, err := r.Register(ctx, agentID, nil, conn); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r.Unregister(ctx, agentID, nil, "not-the-real-session-id")

	online, err := r.IsOnline(ctx, agentID)
	if err != nil {
		t.Fatalf("IsOnline: %v", err)
	}
	if !online {
		t.Error("IsOnline = false after Unregister with a stale sessionID — should have been a no-op")
	}
}

func TestRegister_ASecondRegistrationEvictsTheFirstConnection(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	agentID := uuid.New()
	first := newFakeConn()

	if _, err := r.Register(ctx, agentID, nil, first); err != nil {
		t.Fatalf("Register (first): %v", err)
	}
	// Eviction is fire-and-forget Pub/Sub (both here and in acp_bridge.py):
	// a message published before the first connection's watchForEviction
	// goroutine has actually completed its Subscribe call is silently
	// dropped, not queued. In real usage the several other awaited I/O
	// calls Register makes before this point give that goroutine's first
	// scheduling slot time to run; back-to-back calls in a test with no
	// other work in between don't have that natural buffer, so give it one
	// explicitly rather than produce a flaky test.
	time.Sleep(50 * time.Millisecond)

	second := newFakeConn()
	secondSessionID, err := r.Register(ctx, agentID, nil, second)
	if err != nil {
		t.Fatalf("Register (second): %v", err)
	}
	if secondSessionID == "" {
		t.Error("second Register returned an empty sessionID")
	}

	if !first.wasClosed(t, 2*time.Second) {
		t.Error("the first connection was never closed after a second Register for the same agent")
	}
	if second.closed {
		t.Error("the second (winning) connection should not have been closed")
	}
}

// TestRegister_SecondRegistrationCancelsAndClosesThePreviousEntry is a
// regression test: a same-process reconnect (a second Register for the same
// agent_id) must cancel the old entry's context so its
// forwardDispatchedMessages/watchForEviction goroutines actually exit and
// release their Redis Pub/Sub subscriptions — not just evict the old
// WebSocket connection while leaving those goroutines running forever
// against an orphaned connection. Unlike
// TestRegister_ASecondRegistrationEvictsTheFirstConnection, this doesn't
// need the eviction-broadcast settle time: the cancel-and-close happens
// synchronously inside the second Register call, before it returns.
func TestRegister_SecondRegistrationCancelsAndClosesThePreviousEntry(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	agentID := uuid.New()
	first := newFakeConn()

	if _, err := r.Register(ctx, agentID, nil, first); err != nil {
		t.Fatalf("Register (first): %v", err)
	}

	r.mu.Lock()
	firstEntry := r.connections[agentID]
	r.mu.Unlock()
	if firstEntry == nil {
		t.Fatal("no entry recorded in the registry after the first Register")
	}

	second := newFakeConn()
	if _, err := r.Register(ctx, agentID, nil, second); err != nil {
		t.Fatalf("Register (second): %v", err)
	}

	select {
	case <-firstEntry.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the first entry's forwarder/eviction-watcher goroutines never exited after a second Register — leaked")
	}

	if !first.wasClosed(t, 2*time.Second) {
		t.Error("the first connection was never closed by the second Register")
	}
}

// blockingCloseConn is a Conn whose Close blocks until the test signals
// proceed — used below to hold a Register call inside its
// evict-the-previous-entry step indefinitely, so a concurrent Register for
// a *different* agent_id can be observed either blocking behind it (bug) or
// completing promptly (fixed).
type blockingCloseConn struct {
	*fakeConn
	proceed chan struct{}
}

func newBlockingCloseConn() *blockingCloseConn {
	return &blockingCloseConn{fakeConn: newFakeConn(), proceed: make(chan struct{})}
}

func (b *blockingCloseConn) Close(code int, reason string) error {
	<-b.proceed
	return b.fakeConn.Close(code, reason)
}

// TestRegister_DifferentAgentsDoNotBlockOnEachOthersEviction is a regression
// test for the review finding that registerLocks (formerly a single
// process-wide registerMu) must be keyed per agent_id: Register now waits
// on a superseded connection's own goroutines actually exiting (see
// TestRegister_SecondRegistrationCancelsAndClosesThePreviousEntry above),
// which isn't bounded the way the original presence-check-and-map-write
// critical section was — a single global lock would let one agent's slow
// reconnect stall every other agent's Register on this replica for that
// entire wait.
func TestRegister_DifferentAgentsDoNotBlockOnEachOthersEviction(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	agentA := uuid.New()
	agentB := uuid.New()

	stuck := newBlockingCloseConn()
	if _, err := r.Register(ctx, agentA, nil, stuck); err != nil {
		t.Fatalf("Register (agentA, first): %v", err)
	}

	// A second Register for agentA evicts `stuck`, which blocks inside
	// Close until the test releases it — simulating a slow-to-close
	// connection or slow-to-exit forwarder goroutine.
	agentAUnblocked := make(chan struct{})
	go func() {
		defer close(agentAUnblocked)
		if _, err := r.Register(ctx, agentA, nil, newFakeConn()); err != nil {
			t.Errorf("Register (agentA, second): %v", err)
		}
	}()

	// Give the goroutine above a moment to actually reach the blocked
	// Close call before racing agentB's Register against it.
	select {
	case <-agentAUnblocked:
		t.Fatal("agentA's second Register returned before its blockingCloseConn was released — Close was never actually reached")
	case <-time.After(50 * time.Millisecond):
	}

	agentBDone := make(chan struct{})
	go func() {
		defer close(agentBDone)
		if _, err := r.Register(ctx, agentB, nil, newFakeConn()); err != nil {
			t.Errorf("Register (agentB): %v", err)
		}
	}()

	select {
	case <-agentBDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Register for agentB blocked behind agentA's still-evicting Register — registerLocks is not actually per-agent")
	}

	close(stuck.proceed)
	select {
	case <-agentAUnblocked:
	case <-time.After(2 * time.Second):
		t.Fatal("agentA's second Register never completed after its blockingCloseConn was released")
	}
}

func TestDispatch_ReturnsFalseWithoutPublishingWhenOffline(t *testing.T) {
	r, client := newTestRegistry(t)
	ctx := context.Background()
	agentID := uuid.New()

	sub := client.Subscribe(ctx, dispatchChannel(agentID))
	defer sub.Close()

	delivered, err := r.Dispatch(ctx, agentID, map[string]any{"type": "start_turn"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if delivered {
		t.Error("Dispatch returned true for an agent that was never registered")
	}
}

func TestDispatch_DeliversToARegisteredAgentsDispatchChannel(t *testing.T) {
	r, client := newTestRegistry(t)
	ctx := context.Background()
	agentID := uuid.New()
	conn := newFakeConn()

	if _, err := r.Register(ctx, agentID, nil, conn); err != nil {
		t.Fatalf("Register: %v", err)
	}

	sub := client.Subscribe(ctx, dispatchChannel(agentID))
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	delivered, err := r.Dispatch(ctx, agentID, map[string]any{
		"type": "start_turn", "conversation_id": "c1",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !delivered {
		t.Fatal("Dispatch returned false for a registered agent")
	}

	select {
	case msg := <-sub.Channel():
		var payload map[string]any
		if err := json.Unmarshal([]byte(msg.Payload), &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if payload["type"] != "start_turn" {
			t.Errorf("type = %v, want start_turn", payload["type"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the dispatched message on the raw subscriber")
	}

	// Also confirms Registry's own forwarder (not just a raw subscriber)
	// delivers it to the registered connection.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(conn.messagesSent()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sent := conn.messagesSent()
	if len(sent) != 1 {
		t.Fatalf("connection received %d message(s), want 1: %+v", len(sent), sent)
	}
}

func TestEvict_ClosesTheRegisteredConnectionRegardlessOfSession(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	agentID := uuid.New()
	conn := newFakeConn()

	if _, err := r.Register(ctx, agentID, nil, conn); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// See TestRegister_ASecondRegistrationEvictsTheFirstConnection's
	// comment on this sleep — same fire-and-forget Pub/Sub subscription
	// race, not specific to eviction-via-second-Register.
	time.Sleep(50 * time.Millisecond)
	if err := r.Evict(ctx, agentID); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	if !conn.wasClosed(t, 2*time.Second) {
		t.Error("Evict did not close the registered connection")
	}
	if conn.closeCode != evictionCloseCode {
		t.Errorf("close code = %d, want %d", conn.closeCode, evictionCloseCode)
	}
}

func TestEvict_UnregisteredAgentIsANoOp(t *testing.T) {
	r, _ := newTestRegistry(t)
	if err := r.Evict(context.Background(), uuid.New()); err != nil {
		t.Fatalf("Evict: %v", err)
	}
}

func TestHeartbeat_MissingPresenceRequiresReconnect(t *testing.T) {
	r, _ := newTestRegistry(t)
	if err := r.Heartbeat(context.Background(), uuid.New(), "missing-session"); err == nil {
		t.Fatal("missing presence was accepted as a healthy heartbeat")
	}
}

func TestUnregister_DelayedCleanupCannotDeleteNewPresence(t *testing.T) {
	r, client := newTestRegistry(t)
	ctx := t.Context()
	agentID := uuid.New()
	oldSession, err := r.Register(ctx, agentID, nil, newFakeConn())
	if err != nil {
		t.Fatal(err)
	}
	cleanupStarted, releaseCleanup := make(chan struct{}), make(chan struct{})
	r.mu.Lock()
	entry := r.connections[agentID]
	cancel := entry.cancel
	entry.cancel = func() {
		cancel()
		close(cleanupStarted)
		<-releaseCleanup
	}
	r.mu.Unlock()
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		r.Unregister(context.Background(), agentID, nil, oldSession)
	}()
	<-cleanupStarted
	defer func() {
		close(releaseCleanup)
		<-cleanupDone
	}()
	newSession, err := r.Register(ctx, agentID, nil, newFakeConn())
	if err != nil {
		t.Fatal(err)
	}
	status := client.Subscribe(ctx, messaging.ChannelRealtime)
	defer status.Close()
	if _, err = status.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	// Release the old cleanup before checking the new session's presence.
	releaseCleanup <- struct{}{}
	<-cleanupDone
	defer r.Unregister(context.Background(), agentID, nil, newSession)
	if online, err := r.IsOnline(ctx, agentID); err != nil || !online {
		t.Fatalf("old cleanup deleted new presence: online=%v, error=%v", online, err)
	}
	if ttl := client.TTL(ctx, presenceKey(agentID)).Val(); ttl <= 0 {
		t.Fatalf("new session lost its presence TTL: %v", ttl)
	}
	if err = client.Publish(ctx, messaging.ChannelRealtime, "after-cleanup").Err(); err != nil {
		t.Fatal(err)
	}
	msg, err := status.ReceiveMessage(ctx)
	if err != nil || msg.Payload != "after-cleanup" {
		t.Fatalf("stale cleanup emitted offline for its successor: %v, %v", msg, err)
	}
}

func TestUnregister_ExpiredPresenceStillPublishesOffline(t *testing.T) {
	r, client := newTestRegistry(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	agentID, projectID := uuid.New(), uuid.New()
	session, err := r.Register(ctx, agentID, &projectID, newFakeConn())
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Expire(ctx, presenceKey(agentID), 0).Err(); err != nil {
		t.Fatal(err)
	}
	status := client.Subscribe(ctx, messaging.ChannelRealtime)
	defer status.Close()
	if _, err = status.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	r.Unregister(ctx, agentID, &projectID, session)
	if err = client.Publish(ctx, messaging.ChannelRealtime, "after-cleanup").Err(); err != nil {
		t.Fatal(err)
	}
	msg, err := status.ReceiveMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var event struct {
		Type    string `json:"type"`
		Payload struct {
			ProjectID string `json:"project_id"`
			AgentID   string `json:"agent_id"`
			Connected *bool  `json:"connected"`
		} `json:"payload"`
	}
	if err = json.Unmarshal([]byte(msg.Payload), &event); err != nil {
		t.Fatalf("missing offline event before cleanup completed: %s", msg.Payload)
	}
	if event.Type != "agent.acp_bridge.status" || event.Payload.AgentID != agentID.String() || event.Payload.ProjectID != projectID.String() || event.Payload.Connected == nil || *event.Payload.Connected {
		t.Fatalf("incorrect offline event: %s", msg.Payload)
	}
}

func TestUnregister_SupersededReplicaCannotDeletePresence(t *testing.T) {
	old, client := newTestRegistry(t)
	current := New(client, messaging.NewPublisher(client), old.log)
	ctx := t.Context()
	agentID := uuid.New()
	oldSession, err := old.Register(ctx, agentID, nil, newFakeConn())
	if err != nil {
		t.Fatal(err)
	}
	newSession, err := current.Register(ctx, agentID, nil, newFakeConn())
	if err != nil {
		t.Fatal(err)
	}
	defer current.Unregister(context.Background(), agentID, nil, newSession)
	old.Unregister(ctx, agentID, nil, oldSession)
	if online, err := current.IsOnline(ctx, agentID); err != nil || !online {
		t.Fatalf("superseded replica deleted current presence: online=%v, error=%v", online, err)
	}
}

func TestHeartbeat_SupersededReplicaCannotRefreshPresence(t *testing.T) {
	old, client := newTestRegistry(t)
	current := New(client, messaging.NewPublisher(client), old.log)
	ctx := t.Context()
	agentID := uuid.New()
	oldSession, err := old.Register(ctx, agentID, nil, newFakeConn())
	if err != nil {
		t.Fatal(err)
	}
	defer old.Unregister(context.Background(), agentID, nil, oldSession)
	newSession, err := current.Register(ctx, agentID, nil, newFakeConn())
	if err != nil {
		t.Fatal(err)
	}
	defer current.Unregister(context.Background(), agentID, nil, newSession)
	if err = client.Expire(ctx, presenceKey(agentID), 5*time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	if err = old.Heartbeat(ctx, agentID, oldSession); err == nil {
		t.Error("superseded session heartbeat was accepted")
	}
	if ttl := client.TTL(ctx, presenceKey(agentID)).Val(); ttl != 5*time.Second {
		t.Fatalf("superseded session refreshed current presence TTL: %v", ttl)
	}
}

func TestRelayMessages_ExpiredPresenceClosesSocketForReconnect(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := New(client, messaging.NewPublisher(client), log)
	s := &Server{Registry: r, Log: log}
	agentID := uuid.New()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		session, err := r.Register(req.Context(), agentID, nil, wsConn{conn})
		if err != nil {
			t.Error(err)
			return
		}
		defer r.Unregister(context.Background(), agentID, nil, session)
		if err = wsjson.Write(req.Context(), conn, map[string]string{"type": "hello_ack"}); err != nil {
			return
		}
		s.relayMessages(req.Context(), conn, agentID, session)
	}))
	defer server.Close()
	for attempt := 0; attempt < 2; attempt++ {
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseNow()
		var msg map[string]any
		if err = wsjson.Read(ctx, conn, &msg); err != nil || msg["type"] != "hello_ack" {
			t.Fatalf("registration: %v, %v", msg, err)
		}
		if err = wsjson.Write(ctx, conn, map[string]string{"type": "ping"}); err != nil {
			t.Fatal(err)
		}
		if err = wsjson.Read(ctx, conn, &msg); err != nil || msg["type"] != "pong" {
			t.Fatalf("healthy heartbeat: %v, %v", msg, err)
		}
		if online, err := r.IsOnline(ctx, agentID); err != nil || !online {
			t.Fatalf("registered bridge is offline: %v", err)
		}
		if attempt == 0 {
			mr.FastForward(time.Minute)
			if err = wsjson.Write(ctx, conn, map[string]string{"type": "ping"}); err != nil {
				t.Fatal(err)
			}
			if err = wsjson.Read(ctx, conn, &msg); websocket.CloseStatus(err) != websocket.StatusInternalError {
				t.Fatalf("expired presence must close for reconnect; got %v, %v", msg, err)
			}
		} else {
			if sent, err := r.Dispatch(ctx, agentID, map[string]any{"type": "start_turn"}); err != nil || !sent {
				t.Fatalf("dispatch after reconnect: sent=%v, error=%v", sent, err)
			}
			if err = wsjson.Read(ctx, conn, &msg); err != nil || msg["type"] != "start_turn" {
				t.Fatalf("reconnected bridge did not receive dispatch: %v, %v", msg, err)
			}
		}
	}
}
