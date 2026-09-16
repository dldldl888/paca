package messaging

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/Paca-AI/agent-runner/internal/agent"
)

// decodeTrigger parses one paca:agent:triggers stream entry's flat fields
// (written by Service.publishTrigger/publishChatTrigger/
// publishGlobalChatTrigger via AppendFlat — no JSON envelope) into an
// agent.Trigger. A field simply absent from the map (rather than present-
// but-empty) is how the publisher represents "not applicable to this
// trigger type" — e.g. actor_member_id is omitted entirely for an
// automation-fired task assignment — so every optional lookup below treats
// "missing" and "empty string" the same way: nil.
func decodeTrigger(values map[string]interface{}) (agent.Trigger, error) {
	conversationID, err := requiredUUID(values, "conversation_id")
	if err != nil {
		return agent.Trigger{}, err
	}
	agentID, err := requiredUUID(values, "agent_id")
	if err != nil {
		return agent.Trigger{}, err
	}

	for _, key := range []string{"project_id", "task_id", "comment_id", "chat_session_id", "actor_user_id", "actor_member_id", "environment_id"} {
		if value, present := values[key]; present {
			raw, ok := value.(string)
			if !ok {
				return agent.Trigger{}, fmt.Errorf("invalid trigger field %s", key)
			}
			if raw != "" {
				if _, err := requiredUUID(values, key); err != nil {
					return agent.Trigger{}, err
				}
			}
		}
	}
	// project_id is absent entirely for a global-chat trigger — see
	// publishGlobalChatTrigger — leaving ProjectID at its zero value
	// (uuid.Nil), the same convention agentdom.AgentConversation uses for
	// "no project" throughout this codebase.
	projectID, _ := optionalUUID(values, "project_id")

	triggerType, _ := values["trigger_type"].(string)
	message, _ := values["message"].(string)

	t := agent.Trigger{
		ConversationID: conversationID,
		ProjectID:      projectID,
		AgentID:        agentID,
		TaskID:         optionalUUIDPtr(values, "task_id"),
		CommentID:      optionalUUIDPtr(values, "comment_id"),
		ChatSessionID:  optionalUUIDPtr(values, "chat_session_id"),
		ActorMemberID:  optionalUUIDPtr(values, "actor_member_id"),
		ActorUserID:    optionalUUIDPtr(values, "actor_user_id"),
		TriggerType:    agent.TriggerType(triggerType),
		Message:        message,
	}

	if raw, ok := values["repo_plugin_ids"].(string); ok && raw != "" {
		t.RepoPluginIDs = strings.Split(raw, ",")
	}

	// environment_id/workdir are both resolved server-side by services/api
	// (see agent.Trigger.EnvironmentID's doc comment) and, like every other
	// optional field decoded above, simply absent from values entirely for
	// a trigger with no environment attached — left nil rather than parsed
	// as empty-but-present.
	t.EnvironmentID = optionalUUIDPtr(values, "environment_id")
	if workdir, ok := values["workdir"].(string); ok && workdir != "" {
		t.Workdir = &workdir
	}

	if value, present := values["context_items"]; present {
		raw, ok := value.(string)
		if !ok || json.Unmarshal([]byte(raw), &t.ContextItems) != nil || t.ContextItems == nil {
			return agent.Trigger{}, fmt.Errorf("invalid context_items")
		}
		for _, item := range t.ContextItems {
			if id, e := uuid.Parse(item.ID); e != nil || id == uuid.Nil {
				return agent.Trigger{}, fmt.Errorf("invalid context id")
			}
			if item.ProjectID != nil {
				if id, e := uuid.Parse(*item.ProjectID); e != nil || id == uuid.Nil {
					return agent.Trigger{}, fmt.Errorf("invalid context project")
				}
			}
			switch item.Type {
			case "task", "doc", "conversation", "annotation", "automation":
			default:
				return agent.Trigger{}, fmt.Errorf("invalid context type")
			}
		}
	}

	return t, nil
}

func requiredUUID(values map[string]interface{}, key string) (uuid.UUID, error) {
	raw, _ := values[key].(string)
	if raw == "" {
		return uuid.Nil, fmt.Errorf("messaging: trigger missing required field %q", key)
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, fmt.Errorf("messaging: trigger field %q is not a valid UUID: %w", key, err)
	}
	return id, nil
}

func optionalUUID(values map[string]interface{}, key string) (uuid.UUID, bool) {
	raw, _ := values[key].(string)
	if raw == "" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

func optionalUUIDPtr(values map[string]interface{}, key string) *uuid.UUID {
	id, ok := optionalUUID(values, key)
	if !ok {
		return nil
	}
	return &id
}
