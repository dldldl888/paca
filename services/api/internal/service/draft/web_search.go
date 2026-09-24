package draftsvc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	draftdom "github.com/Paca-AI/api/internal/domain/draft"
	"github.com/Paca-AI/api/internal/platform/netguard"
)

const (
	maxPublicSearchQueryLength = 500
	maxPublicSearchResults     = 50
	maxPublicSearchBody        = 1 << 20
)

var (
	ErrPublicWebSearchUnavailable  = errors.New("public web search unavailable")
	ErrPublicWebSearchQueryInvalid = errors.New("public web search query invalid")
)

// PublicWebSearchClient consumes the JSON result shape exposed by SearXNG.
// The default client is SSRF-safe; tests may inject a client for a local server.
type PublicWebSearchClient struct {
	Endpoint string
	Client   *http.Client
}

func NewPublicWebSearchClient(endpoint string) *PublicWebSearchClient {
	return &PublicWebSearchClient{Endpoint: strings.TrimRight(strings.TrimSpace(endpoint), "/"), Client: netguard.NewSafeHTTPClient(10 * time.Second)}
}

type searxngResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

func (c *PublicWebSearchClient) Search(ctx context.Context, query string, limit int) ([]*draftdom.ProjectDraftWebCandidate, error) {
	if c == nil || strings.TrimSpace(c.Endpoint) == "" {
		return nil, ErrPublicWebSearchUnavailable
	}
	query = strings.TrimSpace(query)
	if query == "" || utf8.RuneCountInString(query) > maxPublicSearchQueryLength {
		return nil, ErrPublicWebSearchQueryInvalid
	}
	if limit < 1 {
		limit = 10
	}
	if limit > maxPublicSearchResults {
		limit = maxPublicSearchResults
	}
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return nil, ErrPublicWebSearchUnavailable
	}
	params := endpoint.Query()
	params.Set("q", query)
	params.Set("format", "json")
	endpoint.RawQuery = params.Encode()
	client := c.Client
	if client == nil {
		client = netguard.NewSafeHTTPClient(10 * time.Second)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, ErrPublicWebSearchUnavailable
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrPublicWebSearchUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, ErrPublicWebSearchUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPublicSearchBody+1))
	if err != nil || len(body) > maxPublicSearchBody {
		return nil, ErrPublicWebSearchUnavailable
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, ErrPublicWebSearchUnavailable
	}
	rawResults, ok := envelope["results"]
	if !ok || len(rawResults) == 0 || strings.TrimSpace(string(rawResults)) == "null" {
		return nil, ErrPublicWebSearchUnavailable
	}
	var decoded searxngResponse
	if err := json.Unmarshal(rawResults, &decoded.Results); err != nil {
		return nil, ErrPublicWebSearchUnavailable
	}
	if len(decoded.Results) == 0 {
		upstreamFailure := false
		for _, key := range []string{"unresponsive_engines", "errors"} {
			raw, ok := envelope[key]
			if !ok || len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
				continue
			}
			var entries []json.RawMessage
			if err := json.Unmarshal(raw, &entries); err != nil {
				return nil, ErrPublicWebSearchUnavailable
			}
			upstreamFailure = upstreamFailure || len(entries) > 0
		}
		if upstreamFailure {
			return nil, ErrPublicWebSearchUnavailable
		}
	}
	items := make([]*draftdom.ProjectDraftWebCandidate, 0, min(limit, len(decoded.Results)))
	accessed := time.Now().UTC()
	for _, result := range decoded.Results {
		if len(items) == limit {
			break
		}
		parsed, err := url.Parse(strings.TrimSpace(result.URL))
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			continue
		}
		items = append(items, &draftdom.ProjectDraftWebCandidate{
			Title: strings.TrimSpace(result.Title), URL: parsed.String(), Snippet: strings.TrimSpace(result.Content),
			SourceType: "web", AccessedAt: accessed, ReadStatus: "unverified", ProcessedStatus: "unverified",
		})
	}
	return items, nil
}

func (s *Service) SearchProjectDraftWeb(ctx context.Context, query string, limit int) ([]*draftdom.ProjectDraftWebCandidate, error) {
	if err := s.requireProjectDraftCapability(ctx); err != nil {
		return nil, err
	}
	ctx, leave, err := s.enterDelivery(ctx)
	if err != nil {
		return nil, err
	}
	defer leave()
	if s.webSearch == nil {
		return nil, ErrPublicWebSearchUnavailable
	}
	items, err := s.webSearch.Search(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Service) WithPublicWebSearch(client *PublicWebSearchClient) *Service {
	s.webSearch = client
	return s
}
