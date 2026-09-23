package draftsvc

import (
	"encoding/json"
	"errors"
	draftdom "github.com/Paca-AI/api/internal/domain/draft"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func validContent() draftdom.Content {
	return draftdom.Content{Goal: "PRIVATE", Items: []draftdom.Item{{ID: "10000000-0000-4000-8000-000000000001", Title: "合成任务", Requirements: "交付", Criteria: "验收", AssigneeMemberID: "20000000-0000-4000-8000-000000000001", DueDate: "2026-12-01T23:59:00+08:00", Importance: 2}}}
}

func TestPublishProjectDraftRequiresHumanSession(t *testing.T) {
	s := &Service{}
	ctx := authz.WithActorID(t.Context(), uuid.New())
	_, err := s.PublishProjectDraft(ctx, uuid.New(), 1, uuid.New())
	if !errors.Is(err, taskdom.ErrTaskForbidden) {
		t.Fatalf("non-human publication was not rejected: %v", err)
	}
}

func TestDraftValidationAndProjection(t *testing.T) {
	for _, test := range []struct {
		name              string
		change            func(*draftdom.Content)
		structural, issue bool
	}{
		{"valid", func(*draftdom.Content) {}, false, false},
		{"empty", func(c *draftdom.Content) { c.Items = nil }, false, true},
		{"missing", func(c *draftdom.Content) { c.Items[0].Criteria = "" }, false, true},
		{"no_offset", func(c *draftdom.Content) { c.Items[0].DueDate = "2026-12-01T23:59:00" }, false, true},
		{"date_only", func(c *draftdom.Content) { c.Items[0].DueDate = "2026-12-01" }, false, true},
		{"start_after", func(c *draftdom.Content) { c.Items[0].StartDate = "2026-12-02T00:00:00+08:00" }, false, true},
		{"invalid_member", func(c *draftdom.Content) { c.Items[0].AssigneeMemberID = "unknown" }, false, true},
		{"self_dependency", func(c *draftdom.Content) { c.Items[0].Dependencies = []string{c.Items[0].ID} }, false, true},
		{"missing_dependency", func(c *draftdom.Content) { c.Items[0].Dependencies = []string{"30000000-0000-4000-8000-000000000001"} }, false, true},
		{"cycle", func(c *draftdom.Content) {
			a := c.Items[0]
			a.ID = "10000000-0000-4000-8000-000000000002"
			a.Dependencies = []string{c.Items[0].ID}
			c.Items[0].Dependencies = []string{a.ID}
			c.Items = append(c.Items, a)
		}, false, true},
		{"duplicate_ids", func(c *draftdom.Content) { c.Items = append(c.Items, c.Items[0]) }, true, false},
		{"canonical_ids", func(c *draftdom.Content) { c.Items[0].ID = "{10000000-0000-4000-8000-000000000001}" }, true, false},
		{"title_limit", func(c *draftdom.Content) { c.Items[0].Title = strings.Repeat("中", 201) }, true, false},
		{"goal_limit", func(c *draftdom.Content) { c.Goal = strings.Repeat("中", 10001) }, true, false},
		{"text_limit", func(c *draftdom.Content) { c.Items[0].Criteria = strings.Repeat("中", 10000) }, true, false},
		{"item_limit", func(c *draftdom.Content) { c.Items = make([]draftdom.Item, 31) }, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := validContent()
			test.change(&c)
			err := c.ValidateStructure()
			if (err != nil) != test.structural {
				t.Fatalf("structure: %v", err)
			}
			if !test.structural {
				p := (&draftdom.Draft{Content: c}).Project()
				if (len(p.Issues) > 0) != test.issue {
					t.Fatal(p)
				}
			}
		})
	}
	c := validContent()
	c.Sources = []string{"PRIVATE_SOURCE"}
	c.Assumptions = []string{"PRIVATE_ASSUMPTION"}
	p := (&draftdom.Draft{Content: c}).Project()
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "PRIVATE") || !p.CanPublish {
		t.Fatal(string(b))
	}
	if !strings.Contains(string(c.Items[0].Description()), "交付") || strings.Contains(string(c.Items[0].Description()), "PRIVATE") {
		t.Fatal("wrong employee projection")
	}
}
func TestGenerationStrictVisibleCompletedTurn(t *testing.T) {
	c := validContent()
	model, _ := json.Marshal(map[string]any{"items": c.Items, "sources": []string{}, "assumptions": []string{}})
	events := func(text string) []visibleEvent {
		chunk, _ := json.Marshal(map[string]any{"content": map[string]any{"type": "text", "text": text}})
		return []visibleEvent{
			{EventType: "user_message", EventSource: "user", Payload: []byte("{}")},
			{EventType: "agent_thought_chunk", EventSource: "agent", Payload: []byte("{invalid private thoughts")},
			{EventType: "agent_message_chunk", EventSource: "agent", Payload: chunk},
			{EventType: "turn_end", EventSource: "system", Payload: []byte(`{"stopReason":"end_turn"}`)},
		}
	}
	out, err := ParseGeneration(events(string(model)), "original private goal")
	if err != nil || len(out.Items) != 1 || out.Goal != "original private goal" {
		t.Fatal(out, err)
	}
	for _, test := range []struct {
		name string
		ev   []visibleEvent
	}{
		{"owner_override", events(`{"items":[],"owner_user_id":"evil"}`)},
		{"project_override", events(`{"items":[],"project_id":"evil"}`)},
		{"trailing_json", events(string(model) + "{}")},
		{"markdown", events("```json\n" + string(model) + "\n```")},
		{"thought_only", []visibleEvent{{EventType: "agent_thought_chunk", EventSource: "agent", Payload: model}}},
		{"continuation", append(events(string(model)), visibleEvent{EventType: "user_message", EventSource: "user", Payload: []byte("{}")})},
		{"incomplete", events(string(model))[:3]},
		{"oversize", events(strings.Repeat("x", MaxBody+1))},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, e := ParseGeneration(test.ev, "goal"); e == nil {
				t.Fatal("accepted untrusted/incorrect generation")
			}
		})
	}
}
