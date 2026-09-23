package draftsvc

import (
	"testing"

	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	"github.com/google/uuid"
)

func projectGenerationEvent(index int, kind, source string, payload map[string]any) *agentdom.AgentConversationEvent {
	return &agentdom.AgentConversationEvent{
		ID:             uuid.New(),
		ConversationID: uuid.New(),
		EventIndex:     index,
		EventType:      kind,
		EventSource:    source,
		Payload:        payload,
	}
}

func TestParseProjectDraftGenerationUsesOneVisibleCompletedTurn(t *testing.T) {
	itemID := uuid.NewString()
	content := `{"name":"新项目","goal":"完成交付","items":[{"id":"` + itemID + `","title":"首项","requirements":"要求","criteria":"标准","dependencies":[]}],"participants":[],"materials":[]}`
	events := []*agentdom.AgentConversationEvent{
		projectGenerationEvent(0, "user_message", "user", map[string]any{"content": map[string]any{"type": "text", "text": "旧轮次"}}),
		projectGenerationEvent(1, "agent_message_chunk", "agent", map[string]any{"content": map[string]any{"type": "text", "text": "不是当前结果"}}),
		projectGenerationEvent(2, "turn_end", "system", map[string]any{"stopReason": "end_turn"}),
		projectGenerationEvent(4, "user_message", "user", map[string]any{"content": map[string]any{"type": "text", "text": "修改目标"}}),
		projectGenerationEvent(5, "agent_thought_chunk", "agent", map[string]any{"content": map[string]any{"type": "text", "text": "secret"}}),
		projectGenerationEvent(6, "agent_message_chunk", "agent", map[string]any{"content": map[string]any{"type": "text", "text": content}}),
		projectGenerationEvent(7, "turn_end", "system", map[string]any{"stopReason": "end_turn"}),
	}
	got, err := ParseProjectDraftGeneration(events, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "新项目" || len(got.Items) != 1 || got.Items[0].ID != itemID {
		t.Fatalf("unexpected parsed draft: %+v", got)
	}
}

func TestParseProjectDraftGenerationRejectsIncompleteOrMultipleTurns(t *testing.T) {
	valid := func() []*agentdom.AgentConversationEvent {
		return []*agentdom.AgentConversationEvent{
			projectGenerationEvent(1, "user_message", "user", map[string]any{}),
			projectGenerationEvent(2, "agent_message_chunk", "agent", map[string]any{"content": map[string]any{"type": "text", "text": `{"name":"新项目","goal":"完成","items":[],"participants":[],"materials":[]}`}}),
			projectGenerationEvent(3, "turn_end", "system", map[string]any{"stopReason": "end_turn"}),
		}
	}
	for _, tc := range []struct {
		name string
		mut  func([]*agentdom.AgentConversationEvent) []*agentdom.AgentConversationEvent
	}{
		{name: "second user", mut: func(events []*agentdom.AgentConversationEvent) []*agentdom.AgentConversationEvent {
			return append(events, projectGenerationEvent(4, "user_message", "user", map[string]any{}))
		}},
		{name: "stopped", mut: func(events []*agentdom.AgentConversationEvent) []*agentdom.AgentConversationEvent {
			events[2].Payload["stopReason"] = "cancelled"
			return events
		}},
		{name: "missing end", mut: func(events []*agentdom.AgentConversationEvent) []*agentdom.AgentConversationEvent {
			return events[:2]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseProjectDraftGeneration(tc.mut(valid()), 0); err == nil {
				t.Fatal("accepted invalid generation")
			}
		})
	}
}
