package acpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Paca-AI/agent-runner/internal/runauth"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
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
