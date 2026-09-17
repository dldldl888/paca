package bootstrap

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestShutdown_DelayedWorkersLeaveTimeForDelivery(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	var finished atomic.Int32
	stops := make([]func() error, 10)
	for i := range stops {
		stops[i] = func() error {
			time.Sleep(80 * time.Millisecond)
			finished.Add(1)
			return nil
		}
	}
	var delivered, publisherClosed bool
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := shutdown(ctx, server.Config, stops, func(ctx context.Context) error {
		delivered = true
		if finished.Load() != 10 {
			return errors.New("delivery stopped before all workers drained")
		}
		return ctx.Err()
	}, func() { publisherClosed = true })
	t.Logf("ten delayed worker stops and HTTP drain took %s", time.Since(start))
	if err != nil || !delivered || !publisherClosed {
		t.Fatalf("shutdown err=%v, delivery called=%v, publisher closed=%v; delivery must receive a live context", err, delivered, publisherClosed)
	}
}

func TestShutdown_DrainsHTTPBeforeDeliveryAndPublisher(t *testing.T) {
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "finished")
		close(finished)
	}))
	defer server.Close()
	response := make(chan error, 1)
	go func() {
		res, err := server.Client().Get(server.URL)
		if err == nil {
			_, err = io.ReadAll(res.Body)
			res.Body.Close()
		}
		response <- err
	}()
	<-entered
	var deliveryCalled, publisherClosed atomic.Bool
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- shutdown(ctx, server.Config, nil, func(ctx context.Context) error {
			deliveryCalled.Store(true)
			select {
			case <-finished:
				return ctx.Err()
			default:
				return errors.New("delivery stopped while HTTP handler still running")
			}
		}, func() { publisherClosed.Store(true) })
	}()
	select {
	case err := <-done:
		t.Errorf("shutdown returned before HTTP handler drained: %v", err)
		close(release)
		<-response
		return
	case <-time.After(60 * time.Millisecond):
	}
	if deliveryCalled.Load() || publisherClosed.Load() {
		t.Error("delivery or publisher closed before HTTP handler drained")
	}
	// Shutdown must already have closed its listener, even though this
	// accepted request is still draining.
	if conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 200*time.Millisecond); err == nil {
		conn.Close()
		t.Error("HTTP listener still accepted requests during shutdown")
	}
	close(release)
	if err := <-response; err != nil {
		t.Fatalf("accepted request did not drain: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !deliveryCalled.Load() || !publisherClosed.Load() {
		t.Fatal("delivery and publisher must close after the accepted request drains")
	}
}

func TestShutdown_WorkerDeadlineKeepsDeliveryAndPublisherOpen(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var delivered, publisherClosed atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- shutdown(ctx, server.Config, []func() error{func() error { <-release; return nil }},
			func(context.Context) error { delivered.Store(true); return nil }, func() { publisherClosed.Store(true) })
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("got %v, want deadline exceeded", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Error("shutdown did not honor its deadline while a worker was blocked")
	}
	if delivered.Load() || publisherClosed.Load() {
		t.Error("undrained worker must keep delivery enrolled and publisher open")
	}
}

func TestShutdown_DrainFailureKeepsDeliveryAndPublisherOpen(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	want := errors.New("worker could not drain")
	var delivered, publisherClosed bool
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := shutdown(ctx, server.Config, []func() error{func() error { return want }},
		func(context.Context) error { delivered = true; return nil }, func() { publisherClosed = true })
	if !errors.Is(err, want) || delivered || publisherClosed {
		t.Fatalf("shutdown err=%v, delivery called=%v, publisher closed=%v", err, delivered, publisherClosed)
	}
}

func TestShutdown_DeliveryFailureKeepsPublisherOpen(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	want := errors.New("could not unregister participant")
	var publisherClosed bool
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := shutdown(ctx, server.Config, nil, func(context.Context) error { return want }, func() { publisherClosed = true })
	if !errors.Is(err, want) || publisherClosed {
		t.Fatalf("shutdown err=%v, publisher closed=%v", err, publisherClosed)
	}
}

func TestShutdown_HTTPDeadlineKeepsDeliveryAndPublisherOpen(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	defer server.Close()
	response := make(chan struct{})
	go func() {
		if res, err := server.Client().Get(server.URL); err == nil {
			res.Body.Close()
		}
		close(response)
	}()
	<-entered
	defer func() { close(release); <-response }()
	var delivered, publisherClosed bool
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := shutdown(ctx, server.Config, nil, func(context.Context) error { delivered = true; return nil }, func() { publisherClosed = true })
	if !errors.Is(err, context.DeadlineExceeded) || delivered || publisherClosed {
		t.Fatalf("shutdown err=%v, delivery called=%v, publisher closed=%v", err, delivered, publisherClosed)
	}
}

func TestShutdown_DeliveryDeadlineKeepsPublisherOpen(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	release := make(chan struct{})
	defer close(release)
	var delivered, publisherClosed atomic.Bool
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- shutdown(ctx, server.Config, nil, func(ctx context.Context) error {
			delivered.Store(true)
			<-release // Models Gate.Disconnect waiting on a protected lease.
			return ctx.Err()
		}, func() { publisherClosed.Store(true) })
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("got %v, want deadline exceeded", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Error("shutdown did not honor its deadline while delivery was blocked")
	}
	if !delivered.Load() || publisherClosed.Load() {
		t.Errorf("delivery called=%v, publisher closed=%v", delivered.Load(), publisherClosed.Load())
	}
}
