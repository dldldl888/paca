package handler

import (
	"bytes"
	"context"
	attachmentdom "github.com/Paca-AI/api/internal/domain/attachment"
	"github.com/Paca-AI/api/internal/platform/delivery"
	"github.com/Paca-AI/api/internal/platform/storage"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type streamStore struct {
	storage.Client
	open func(context.Context, string) (*storage.Object, error)
}

func (s streamStore) OpenObject(ctx context.Context, _, _, rg string) (*storage.Object, error) {
	return s.open(ctx, rg)
}

type delayedRead struct {
	first   bool
	next    chan struct{}
	entered chan struct{}
}

func (s *delayedRead) Read(p []byte) (int, error) {
	if !s.first {
		s.first = true
		return copy(p, bytes.Repeat([]byte("a"), 64*1024)), nil
	}
	close(s.entered)
	<-s.next
	return copy(p, []byte("SECRET_LATER")), io.EOF
}
func (*delayedRead) Close() error { return nil }
func fileFixture() *attachmentdom.File {
	return &attachmentdom.File{FileName: "secret-name.html", ContentType: "text/html", StorageKey: "secret", Bucket: "test"}
}
func TestFileDeliveryEpochChangeBeforeHeaders(t *testing.T) {
	for _, method := range []string{"GET", "HEAD"} {
		t.Run(method, func(t *testing.T) {
			gate := delivery.NewGate()
			_ = gate.Update("1", false)
			store := streamStore{open: func(context.Context, string) (*storage.Object, error) {
				_ = gate.Close("2")
				_ = gate.Update("2", false)
				return &storage.Object{Body: io.NopCloser(strings.NewReader("secret")), Size: 6, ContentRange: "bytes 0-5/99"}, nil
			}}
			d := NewFileDelivery(gate, store, "test", nil)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				d.serve(w, r, "", func(context.Context) (*attachmentdom.File, error) { return fileFixture(), nil }, false)
			}))
			defer srv.Close()
			req, _ := http.NewRequest(method, srv.URL, nil)
			req.Header.Set("Range", "bytes=0-5")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 503 || strings.Contains(string(b), "secret") || resp.Header.Get("Content-Disposition") != "" || resp.Header.Get("Content-Range") != "" {
				t.Fatalf("stale metadata/bytes leaked: %d %v %s", resp.StatusCode, resp.Header, b)
			}
		})
	}
}
func TestFileDeliveryReleasesLeaseDuringReadAndReentersOriginalContext(t *testing.T) {
	gate := delivery.NewGate()
	_ = gate.Update("1", false)
	rd := &delayedRead{next: make(chan struct{}), entered: make(chan struct{})}
	store := streamStore{open: func(context.Context, string) (*storage.Object, error) {
		return &storage.Object{Body: rd, Size: 64*1024 + 12}, nil
	}}
	d := NewFileDelivery(gate, store, "test", nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.serve(w, r, "", func(context.Context) (*attachmentdom.File, error) { return fileFixture(), nil }, false)
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-rd.entered
	done := make(chan struct{})
	go func() { _ = gate.Close("2"); _ = gate.Update("2", false); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(rd.next)
		t.Fatal("slow object read retained gate lease")
	}
	close(rd.next)
	b, _ := io.ReadAll(resp.Body)
	if len(b) != 64*1024 || bytes.Contains(b, []byte("SECRET_LATER")) {
		t.Fatalf("post-revocation bytes: %d", len(b))
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") || resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatal(resp.Header)
	}
}

// The writer announces the actual lease-held write, then delegates to a real
// socket with tiny buffers whose client deliberately never reads the body.
type observeFileWrite struct {
	http.ResponseWriter
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *observeFileWrite) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observeFileWrite) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.ResponseWriter.Write(p)
}
func TestFileDeliverySlowClientBoundsRevocation(t *testing.T) {
	gate := delivery.NewGate()
	_ = gate.Update("1", false)
	store := streamStore{open: func(context.Context, string) (*storage.Object, error) {
		return &storage.Object{Body: io.NopCloser(io.LimitReader(zeroReader{}, 64<<20)), Size: 64 << 20}, nil
	}}
	d := NewFileDelivery(gate, store, "test", nil)
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.serve(&observeFileWrite{ResponseWriter: w, entered: entered, release: release}, r, "", func(context.Context) (*attachmentdom.File, error) { return fileFixture(), nil }, false)
	}))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			_ = c.(*net.TCPConn).SetWriteBuffer(1024)
		}
	}
	srv.Start()
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.(*net.TCPConn).SetReadBuffer(1024)
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")
	<-entered
	done := make(chan struct{})
	go func() { _ = gate.Close("2"); close(done) }()
	close(release)
	select {
	case <-done:
	case <-time.After(fileWriteTimeout + 2*time.Second):
		t.Fatal("slow client indefinitely held authorization transition")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
