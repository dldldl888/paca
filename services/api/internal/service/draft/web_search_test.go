package draftsvc

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicWebSearchClientReportsMissingProvider(t *testing.T) {
	client := NewPublicWebSearchClient("")
	_, err := client.Search(t.Context(), "public query", 5)
	if !errors.Is(err, ErrPublicWebSearchUnavailable) {
		t.Fatalf("expected unavailable provider, got %v", err)
	}
}

func TestPublicWebSearchClientRejectsLoopbackProvider(t *testing.T) {
	client := NewPublicWebSearchClient("http://127.0.0.1:1/search")
	_, err := client.Search(t.Context(), "public query", 5)
	if !errors.Is(err, ErrPublicWebSearchUnavailable) {
		t.Fatalf("expected loopback provider to be unavailable, got %v", err)
	}
}

func TestPublicWebSearchClientParsesSearXNGResultsWithMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "public query" || r.URL.Query().Get("format") != "json" {
			t.Fatalf("unexpected upstream query: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Public page","url":"https://example.com/page","content":"summary"}]}`))
	}))
	defer server.Close()

	client := &PublicWebSearchClient{Endpoint: server.URL, Client: server.Client()}
	items, err := client.Search(t.Context(), "public query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].URL != "https://example.com/page" || items[0].ReadStatus != "unverified" || items[0].AccessedAt.IsZero() {
		t.Fatalf("unexpected public candidate: %#v", items)
	}
}

func TestPublicWebSearchClientRejectsMalformedAndBoundsInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"one","url":"https://one.example"},{"title":"two","url":"https://two.example"}]}`))
	}))
	defer server.Close()
	client := &PublicWebSearchClient{Endpoint: server.URL, Client: server.Client()}
	_, err := client.Search(t.Context(), strings.Repeat("q", maxPublicSearchQueryLength+1), 99)
	if !errors.Is(err, ErrPublicWebSearchQueryInvalid) {
		t.Fatalf("expected query validation error, got %v", err)
	}
	client.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	if _, err := client.Search(t.Context(), "query", 5); !errors.Is(err, ErrPublicWebSearchUnavailable) {
		t.Fatalf("expected malformed upstream to be unavailable, got %v", err)
	}
}

func TestPublicWebSearchClientRejectsMissingOrNullResultsOnHTTP200(t *testing.T) {
	for _, payload := range []string{`{}`, `{"results":null}`} {
		t.Run(payload, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(payload))
			}))
			defer server.Close()
			client := &PublicWebSearchClient{Endpoint: server.URL, Client: server.Client()}
			if _, err := client.Search(t.Context(), "query", 5); !errors.Is(err, ErrPublicWebSearchUnavailable) {
				t.Fatalf("expected missing result array to be unavailable, got %v", err)
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()
	client := &PublicWebSearchClient{Endpoint: server.URL, Client: server.Client()}
	items, err := client.Search(t.Context(), "query", 5)
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("valid empty result set should remain an empty success: items=%#v err=%v", items, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
