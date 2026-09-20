// Package runauth delegates current private run authorization to the API.
package runauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Paca-AI/agent-runner/internal/agent"
	"github.com/google/uuid"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Client struct {
	Origin, InternalKey, InstanceID string
	HTTP                            *http.Client
	Gate                            *Gate
}

func New(origin, key, instance string) *Client {
	return &Client{Origin: strings.TrimRight(origin, "/"), InternalKey: key, InstanceID: instance, HTTP: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type Authorization struct {
	AuthorizationEpoch string `json:"authorization_epoch"`
	ConversationID     string `json:"conversation_id"`
	AgentID            string `json:"agent_id"`
	InstanceID         string `json:"instance_id"`
	ProjectID          string `json:"project_id"`
	Audience           string `json:"audience"`
	OwnerUserID        string `json:"owner_user_id"`
	HasPriorTurns      bool   `json:"has_prior_turns"`
	Status             string `json:"status"`
	TaskID             string `json:"task_id"`
	CommentID          string `json:"comment_id"`
	ChatSessionID      string `json:"chat_session_id"`
}

func (c *Client) Authorize(ctx context.Context, conversation, agentID uuid.UUID, begin bool) (*Authorization, error) {
	if c == nil || c.InternalKey == "" || c.InstanceID == "" {
		return nil, ErrUnavailable
	}
	body, _ := json.Marshal(map[string]any{"conversation_id": conversation.String(), "agent_id": agentID.String(), "begin_turn": begin})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Origin+"/api/internal/agent-runs/authorize", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Internal-Token", c.InternalKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == 409 {
			return nil, ErrBlocked
		}
		if resp.StatusCode == 404 || resp.StatusCode == 400 {
			return nil, ErrDenied
		}
		return nil, ErrUnavailable
	}
	var envelope struct {
		Success   bool            `json:"success"`
		Data      json.RawMessage `json:"data"`
		RequestID string          `json:"request_id"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 8192))
	decoder.DisallowUnknownFields()
	var extra any
	if err = decoder.Decode(&envelope); err != nil || !envelope.Success || decoder.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("invalid authorization envelope")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(envelope.Data, &fields) != nil || len(fields) != 12 {
		return nil, fmt.Errorf("incomplete authorization")
	}
	for _, key := range []string{"authorization_epoch", "conversation_id", "agent_id", "instance_id", "project_id", "audience", "owner_user_id", "has_prior_turns", "status", "task_id", "comment_id", "chat_session_id"} {
		raw, ok := fields[key]
		if !ok || string(raw) == "null" {
			return nil, fmt.Errorf("incomplete authorization")
		}
	}
	// Every identity/status field is required, including the current delivery epoch.
	var a Authorization
	d := json.NewDecoder(bytes.NewReader(envelope.Data))
	d.DisallowUnknownFields()
	if d.Decode(&a) != nil {
		return nil, fmt.Errorf("invalid authorization")
	}
	if n, e := strconv.ParseUint(a.AuthorizationEpoch, 10, 63); e != nil || strconv.FormatUint(n, 10) != a.AuthorizationEpoch {
		return nil, ErrUnavailable
	}
	if a.ConversationID != conversation.String() || a.AgentID != agentID.String() || a.InstanceID != c.InstanceID || a.Audience != "owner_private" {
		return nil, fmt.Errorf("authorization identity mismatch")
	}
	for _, value := range []string{a.ConversationID, a.AgentID, a.InstanceID, a.OwnerUserID} {
		if id, e := uuid.Parse(value); e != nil || id == uuid.Nil {
			return nil, fmt.Errorf("invalid authorization identity")
		}
	}
	for _, value := range []string{a.ProjectID, a.TaskID, a.CommentID, a.ChatSessionID} {
		if value != "" {
			if id, e := uuid.Parse(value); e != nil || id == uuid.Nil {
				return nil, fmt.Errorf("invalid authorization linkage")
			}
		}
	}
	switch a.Status {
	case "queued", "running", "paused", "finished", "failed", "stopped":
	default:
		return nil, fmt.Errorf("invalid authorization status")
	}
	if begin && a.Status != "running" {
		return nil, fmt.Errorf("conversation not running")
	}
	return &a, nil
}

// Apply overwrites every security-sensitive queue field from the authenticated response.
func (a *Authorization) Apply(t *agent.Trigger) {
	t.AgentID = uuid.MustParse(a.AgentID)
	t.ProjectID = uuid.Nil
	if a.ProjectID != "" {
		t.ProjectID = uuid.MustParse(a.ProjectID)
	}
	owner := uuid.MustParse(a.OwnerUserID)
	t.ActorUserID = &owner
	t.ActorMemberID = nil
	t.TaskID = parseOptional(a.TaskID)
	t.CommentID = parseOptional(a.CommentID)
	t.ChatSessionID = parseOptional(a.ChatSessionID)
}
func parseOptional(value string) *uuid.UUID {
	if value == "" {
		return nil
	}
	id := uuid.MustParse(value)
	return &id
}

var ErrUnavailable = errors.New("run authorization unavailable")
var ErrDenied = errors.New("run authorization refused")

// CompanyContext is separate from the fixed twelve-field authorization protocol.
// It is fetched within the dispatch gate and bound to the same trusted identity.
type CompanySource struct {
	DocumentID       string `json:"document_id"`
	ProjectID        string `json:"project_id"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	UpdatedAt        string `json:"updated_at"`
	Truncated        bool   `json:"truncated"`
	SourceDocumentID string `json:"source_document_id,omitempty"`
	SourceProjectID  string `json:"source_project_id,omitempty"`
	SourceTitle      string `json:"source_title,omitempty"`
}
type CompanyContext struct {
	AuthorizationEpoch string          `json:"authorization_epoch"`
	ConversationID     string          `json:"conversation_id"`
	AgentID            string          `json:"agent_id"`
	InstanceID         string          `json:"instance_id"`
	OwnerUserID        string          `json:"owner_user_id"`
	Text               string          `json:"text"`
	Sources            []CompanySource `json:"sources"`
	Truncated          bool            `json:"truncated"`
}

func (c *Client) CompanyContext(ctx context.Context, a *Authorization) (*CompanyContext, error) {
	if c == nil || a == nil || c.InternalKey == "" || a.InstanceID != c.InstanceID || a.Audience != "owner_private" {
		return nil, ErrUnavailable
	}
	body, _ := json.Marshal(map[string]string{"conversation_id": a.ConversationID, "agent_id": a.AgentID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Origin+"/api/internal/agent-runs/company-context", bytes.NewReader(body))
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("X-Internal-Token", c.InternalKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if resp.StatusCode == 409 {
			return nil, ErrBlocked
		}
		if resp.StatusCode == 400 || resp.StatusCode == 404 {
			return nil, ErrDenied
		}
		return nil, ErrUnavailable
	}
	var envelope struct {
		Success   bool            `json:"success"`
		Data      json.RawMessage `json:"data"`
		RequestID string          `json:"request_id"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 512<<10))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&envelope) != nil || !envelope.Success || decoder.Decode(&extra) != io.EOF {
		return nil, ErrUnavailable
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(envelope.Data, &fields) != nil || len(fields) != 8 {
		return nil, ErrUnavailable
	}
	for _, key := range []string{"authorization_epoch", "conversation_id", "agent_id", "instance_id", "owner_user_id", "text", "sources", "truncated"} {
		raw, ok := fields[key]
		if !ok || string(raw) == "null" {
			return nil, ErrUnavailable
		}
	}
	var result CompanyContext
	d := json.NewDecoder(bytes.NewReader(envelope.Data))
	d.DisallowUnknownFields()
	if d.Decode(&result) != nil {
		return nil, ErrUnavailable
	}
	if result.AuthorizationEpoch != a.AuthorizationEpoch || result.ConversationID != a.ConversationID || result.AgentID != a.AgentID || result.InstanceID != a.InstanceID || result.OwnerUserID != a.OwnerUserID {
		return nil, ErrUnavailable
	}
	if result.Text == "" || utf8.RuneCountInString(result.Text) > 32000 || len(result.Sources) > 20 {
		return nil, ErrUnavailable
	}
	for _, source := range result.Sources {
		if source.SourceDocumentID != "" || source.SourceProjectID != "" || source.SourceTitle != "" {
			canonical, e1 := uuid.Parse(source.SourceDocumentID)
			project, e2 := uuid.Parse(source.SourceProjectID)
			if e1 != nil || e2 != nil || canonical == uuid.Nil || project == uuid.Nil || source.SourceTitle == "" || utf8.RuneCountInString(source.SourceTitle) > 256 {
				return nil, ErrUnavailable
			}
		}
		doc, e1 := uuid.Parse(source.DocumentID)
		project, e2 := uuid.Parse(source.ProjectID)
		_, e3 := time.Parse(time.RFC3339, source.UpdatedAt)
		if e1 != nil || e2 != nil || e3 != nil || doc == uuid.Nil || project == uuid.Nil || utf8.RuneCountInString(source.Title) > 256 || source.URL != "/projects/"+project.String()+"/docs/"+doc.String() {
			return nil, ErrUnavailable
		}
	}
	return &result, nil
}
