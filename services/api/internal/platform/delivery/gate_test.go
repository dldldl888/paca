package delivery

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCopyLeaseLetsFreshContextReuseOuterLease(t *testing.T) {
	g := NewGate()
	if err := g.Update("1", false); err != nil {
		t.Fatal(err)
	}
	outer, _, leave, err := g.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(leave) }
	defer release()

	closed := make(chan struct{})
	go func() { _ = g.Close("2"); close(closed) }()
	deadline := time.After(time.Second)
	for !g.State().Blocked {
		select {
		case <-deadline:
			t.Fatal("revocation did not begin")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	entered := make(chan error, 1)
	go func() {
		_, _, nestedLeave, nestedErr := g.Enter(CopyLease(context.Background(), outer))
		nestedLeave()
		entered <- nestedErr
	}()
	select {
	case err := <-entered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fresh actor context tried to acquire a nested lease")
	}
	select {
	case <-closed:
		t.Fatal("revocation committed before the outer lease was released")
	default:
	}
	release()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("revocation did not commit after the outer lease was released")
	}
}
