package acpbridge

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/Paca-AI/agent-runner/internal/agent"
	"github.com/Paca-AI/agent-runner/internal/messaging"
	"github.com/Paca-AI/agent-runner/internal/repository/postgres"
	"github.com/Paca-AI/agent-runner/internal/runauth"
)

const (
	offlineMessage = "助手当前离线，请联系管理员检查连接后重试。"
	timeoutMessage = "助手响应超时，请稍后重试；若持续发生，请联系管理员。"
)

// Dispatcher routes triggers for acp-type agents to their connected local
// bridge — mirrors agent/acp_dispatch.py's role for LLM agents
// (executor.Run/handler.Handler), but never spins up a sandbox: the ACP CLI
// runs entirely on the user's own machine via apps/acp-bridge, connected
// through Registry/server.go. Auth, MCP servers, and skills are all the
// user's own local responsibility — nothing is forwarded from Paca.
type Dispatcher struct {
	RunAuthorizer *runauth.Client
	Registry      *Registry
	ConvRepo      *postgres.ConversationRepository
	Publisher     *messaging.Publisher
	Log           Logger
}

// DispatchTrigger hands trigger off to cfg's connected local ACP bridge.
// Mirrors dispatch_acp_trigger: fails the conversation immediately if the
// bridge isn't connected (checked, then re-checked after the dispatch
// publish itself — Valkey Pub/Sub has no delivery guarantee, so a bridge
// that disconnects between the two checks needs the same failure path),
// otherwise schedules a watchdog to fail the conversation if the bridge
// never reports a terminal turn_status within its timeout.
func (d *Dispatcher) DispatchTrigger(ctx context.Context, trigger agent.Trigger, cfg *agent.ACPConfig) error {
	ctx, epoch, leave, gateErr := d.RunAuthorizer.Enter(ctx)
	if gateErr != nil {
		return gateErr
	}
	defer leave()
	online, err := d.Registry.IsOnline(ctx, cfg.ID)
	if err != nil {
		return err
	}
	if !online {
		d.Log.Info("acpbridge: bridge offline; failing conversation",
			"agent_id", cfg.ID, "conversation_id", trigger.ConversationID)
		return d.failOffline(ctx, trigger)
	}

	// Explicit persisted Hermes configuration; other custom ACP agents retain their provider behavior.
	companyAssistant := cfg.ACPProvider == "custom" && cfg.Handle == "hermes"
	var authorization *runauth.Authorization
	if d.RunAuthorizer != nil {
		authorization, err = d.RunAuthorizer.Authorize(ctx, trigger.ConversationID, cfg.ID, !companyAssistant)
		if errors.Is(err, runauth.ErrUnavailable) {
			return err
		}
		if err == nil && epoch != "" && authorization.AuthorizationEpoch != epoch {
			return runauth.ErrUnavailable
		}
		if err != nil {
			if _, e := d.ConvRepo.FailIfNotTerminal(ctx, trigger.ConversationID, "Private run authorization refused"); e != nil {
				return e
			}
			return d.Publisher.PublishConversationStatus(ctx, trigger.ConversationID, "failed")
		}
		authorization.Apply(&trigger)
	} else if err := d.ConvRepo.UpdateStatus(ctx, trigger.ConversationID, "running", nil); err != nil {
		return err
	}

	message := BuildACPMessage(trigger, cfg.ACPProvider)
	if authorization != nil && companyAssistant {
		company, contextErr := d.RunAuthorizer.CompanyContext(ctx, authorization)
		if contextErr != nil {
			return d.failCompanyContext(ctx, trigger)
		}

		// A failed context fetch must not claim a native turn that never existed.
		begun, beginErr := d.RunAuthorizer.Authorize(ctx, trigger.ConversationID, cfg.ID, true)
		if beginErr != nil {
			return d.failCompanyContext(ctx, trigger)
		}
		identity := *begun
		identity.HasPriorTurns = authorization.HasPriorTurns
		if identity != *authorization {
			return d.failCompanyContext(ctx, trigger)
		}
		authorization = begun
		message = BuildPrivateCompanyMessage(trigger, company.Text)
	}
	payload := map[string]any{
		"type":            "start_turn",
		"conversation_id": trigger.ConversationID.String(),
		"project_id":      projectIDOrEmpty(trigger.ProjectID),
		"message":         message,
		"trigger_type":    string(trigger.TriggerType),
		"acp_provider":    cfg.ACPProvider,
		"acp_command":     cfg.ACPCommand,
		// Task/comment/chat linkage the bridge uses to group conversations
		// into per-task (or per-chat) sessions. Empty when the trigger has no
		// such link (e.g. project-global chat, or an automation message with
		// no task); an older bridge simply ignores these keys.
		"task_id":         uuidPtrOrEmpty(trigger.TaskID),
		"comment_id":      uuidPtrOrEmpty(trigger.CommentID),
		"chat_session_id": uuidPtrOrEmpty(trigger.ChatSessionID),
	}
	if authorization != nil {
		payload["authorization_epoch"] = authorization.AuthorizationEpoch
		payload["agent_id"] = authorization.AgentID
		payload["instance_id"] = authorization.InstanceID
		payload["audience"] = authorization.Audience
		payload["owner_user_id"] = authorization.OwnerUserID
		payload["has_prior_turns"] = authorization.HasPriorTurns
	}
	dispatched, err := d.Registry.Dispatch(ctx, cfg.ID, payload)
	if err != nil {
		return err
	}
	if !dispatched {
		// Bridge disconnected between the IsOnline check above and here
		// (e.g. the daemon crashed mid-dispatch) — fail the same way.
		d.Log.Info("acpbridge: bridge went offline before dispatch; failing conversation",
			"agent_id", cfg.ID, "conversation_id", trigger.ConversationID)
		return d.failOffline(ctx, trigger)
	}

	timeoutMinutes := cfg.TimeoutMinutes
	if timeoutMinutes <= 0 {
		timeoutMinutes = 1
	}
	go d.watchdog(trigger.ConversationID, trigger.ProjectID, time.Duration(timeoutMinutes)*time.Minute, trigger.ActorUserID)
	return nil
}

func (d *Dispatcher) failOffline(ctx context.Context, trigger agent.Trigger) error {
	errMsg := offlineMessage
	if err := d.ConvRepo.UpdateStatus(ctx, trigger.ConversationID, "failed", &errMsg); err != nil {
		return err
	}
	if err := d.Publisher.PublishRealtime(ctx, trigger.ProjectID, trigger.ConversationID,
		"agent.conversation.failed", nil, trigger.ActorUserID); err != nil {
		d.Log.Warn("acpbridge: failed to publish realtime status",
			"conversation_id", trigger.ConversationID, "error", err)
	}
	return d.Publisher.PublishConversationStatus(ctx, trigger.ConversationID, "failed")
}

// watchdog fails a dispatched ACP turn that never reported a terminal
// turn_status. Dispatch to the bridge goes over Valkey Pub/Sub (see
// Registry.Dispatch), which has no delivery guarantee — a start_turn
// published while the daemon is mid-reconnect is silently dropped, leaving
// the conversation stuck at "running" with nothing to ever move it forward.
// This is the backstop: after timeout, fail the conversation if it hasn't
// already reached a terminal status. Race-safe against a legitimate late
// turn_status via ConvRepo.FailIfNotTerminal's conditional UPDATE.
//
// Runs detached from the request that scheduled it (mirrors
// asyncio.create_task, not tied to the caller's own context) — a long-lived
// timer that must survive after DispatchTrigger itself has already
// returned.
func (d *Dispatcher) watchdog(conversationID, projectID uuid.UUID, timeout time.Duration, actorUserID *uuid.UUID) {
	time.Sleep(timeout)
	ctx := context.Background()
	failed, err := d.ConvRepo.FailIfNotTerminal(ctx, conversationID, timeoutMessage)
	if err != nil {
		d.Log.Warn("acpbridge: watchdog failed to check/update conversation status",
			"conversation_id", conversationID, "error", err)
		return
	}
	if !failed {
		return
	}
	d.Log.Warn("acpbridge: ACP turn timed out with no turn_status from the bridge",
		"conversation_id", conversationID, "timeout", timeout)
	if err := d.Publisher.PublishRealtime(ctx, projectID, conversationID,
		"agent.conversation.failed", nil, actorUserID); err != nil {
		d.Log.Warn("acpbridge: failed to publish realtime status",
			"conversation_id", conversationID, "error", err)
	}
}

func projectIDOrEmpty(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

func uuidPtrOrEmpty(id *uuid.UUID) string {
	if id == nil || *id == uuid.Nil {
		return ""
	}
	return id.String()
}

// This only terminates a run; it does not mutate authority. A delivery close may
// wait on our lease while this conditional update preserves any existing terminal.
func (d *Dispatcher) failCompanyContext(ctx context.Context, trigger agent.Trigger) error {
	failed, err := d.ConvRepo.FailIfNotTerminal(ctx, trigger.ConversationID, "公司资料暂时无法读取，请稍后重试。")
	if err != nil {
		return err
	}
	if !failed {
		return nil
	}
	if err = d.Publisher.PublishRealtime(ctx, trigger.ProjectID, trigger.ConversationID, "agent.conversation.failed", nil, trigger.ActorUserID); err != nil {
		d.Log.Warn("acpbridge: failed to publish company context failure", "conversation_id", trigger.ConversationID, "error", err)
	}
	return d.Publisher.PublishConversationStatus(ctx, trigger.ConversationID, "failed")
}
