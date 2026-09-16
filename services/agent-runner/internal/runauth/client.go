// Package runauth delegates current private run authorization to the API.
package runauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Paca-AI/agent-runner/internal/agent"
	"github.com/google/uuid"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	Origin, InternalKey, InstanceID string
	HTTP                            *http.Client
}

func New(origin, key, instance string) *Client {
	return &Client{Origin: strings.TrimRight(origin, "/"), InternalKey: key, InstanceID: instance, HTTP: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type Authorization struct {
	ConversationID string `json:"conversation_id"`
	AgentID        string `json:"agent_id"`
	InstanceID     string `json:"instance_id"`
	ProjectID      string `json:"project_id"`
	Audience       string `json:"audience"`
	OwnerUserID    string `json:"owner_user_id"`
	HasPriorTurns  bool   `json:"has_prior_turns"`
	Status         string `json:"status"`
	TaskID         string `json:"task_id"`
	CommentID      string `json:"comment_id"`
	ChatSessionID  string `json:"chat_session_id"`
}

func (c *Client) Authorize(ctx context.Context, conversation, agentID uuid.UUID, begin bool) (*Authorization, error) {
	if c == nil || c.InternalKey == "" || c.InstanceID == "" {
		return nil, fmt.Errorf("private run authorization unavailable")
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
		return nil, fmt.Errorf("private run authorization unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("private run authorization refused (%d)", resp.StatusCode)
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
	if json.Unmarshal(envelope.Data, &fields) != nil || len(fields) != 11 {
		return nil, fmt.Errorf("incomplete authorization")
	}
	for _, key := range []string{"conversation_id", "agent_id", "instance_id", "project_id", "audience", "owner_user_id", "has_prior_turns", "status", "task_id", "comment_id", "chat_session_id"} {
		raw, ok := fields[key]
		if !ok || string(raw) == "null" {
			return nil, fmt.Errorf("incomplete authorization")
		}
	}
	// There are exactly eleven identity/status fields; reject unknown fields too.
	var a Authorization
	d := json.NewDecoder(bytes.NewReader(envelope.Data))
	d.DisallowUnknownFields()
	if d.Decode(&a) != nil {
		return nil, fmt.Errorf("invalid authorization")
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
