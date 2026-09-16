package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	automationdom "github.com/Paca-AI/api/internal/domain/automation"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/events"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type oneShotStepFailureRepo struct {
	*fakePauseResumeRepo
	failed    bool
	stateErr  error
	deleteErr error
}

func (r *oneShotStepFailureRepo) CreateRunStep(ctx context.Context, s *automationdom.RunStep) error {
	if !r.failed {
		r.failed = true
		return errors.New("one-shot step write failure")
	}
	return r.fakePauseResumeRepo.CreateRunStep(ctx, s)
}
func (r *oneShotStepFailureRepo) UpdateRun(ctx context.Context, run *automationdom.Run) error {
	if r.stateErr != nil {
		return r.stateErr
	}
	return r.fakePauseResumeRepo.UpdateRun(ctx, run)
}

func TestAutomationStepPersistenceFailure(t *testing.T) {
	for _, kind := range []string{"wait", "agent", "fanout", "action"} {
		for _, fail := range []bool{true, false} {
			name := kind + "/control"
			if fail {
				name = kind + "/one_shot_failure"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				var c *AutomationConsumer
				var repo *fakePauseResumeRepo
				var updater *fakeTaskUpdater
				var runID uuid.UUID
				var task *taskdom.Task
				if kind == "agent" || kind == "fanout" {
					c, _, repo, updater, runID, _, task = newPauseResumeFixture(t)
					c.agentMessenger.(*fakeAgentMessenger).taskCalls = 0
					if kind == "fanout" {
						var cfg automationdom.ActionConfig
						_ = json.Unmarshal(repo.nodes[0].Config, &cfg)
						cfg.Target = &automationdom.TaskTarget{Kind: automationdom.TaskTargetChildren}
						repo.nodes[0].Config, _ = json.Marshal(cfg)
						reader := c.taskRepo.(*fakeAutomationTaskReader)
						children := []*taskdom.Task{{ID: uuid.New(), ProjectID: task.ProjectID, ParentTaskID: &task.ID}, {ID: uuid.New(), ProjectID: task.ProjectID, ParentTaskID: &task.ID}}
						reader.children = map[uuid.UUID][]*taskdom.Task{task.ID: children}
						for _, child := range children {
							reader.byID[child.ID] = child
						}
					}
				} else {
					c, repo, updater, runID = newWaitPauseFixture(t)
					task = c.taskRepo.(*fakeAutomationTaskReader).byID[*repo.runs[runID].TaskID]
					if kind == "action" {
						repo.nodes[0].Type = repo.nodes[1].Type
						repo.nodes[0].Config = json.RawMessage(`{"update":{"title":"first action applied"}}`)
					}
				}
				repo.pendingDelays = nil
				repo.pendingWaits = nil
				repo.runSteps = nil
				fault := &oneShotStepFailureRepo{fakePauseResumeRepo: repo}
				if fail {
					c.repo = fault
				}
				nodes, outgoing := indexGraph(&automationdom.Graph{Nodes: repo.nodes, Edges: repo.edges})
				w := &walker{consumer: c, projectID: repo.automation.ProjectID, task: task, runID: runID, nodesByID: nodes, outgoing: outgoing, visited: map[uuid.UUID]bool{}}
				w.walk(ctx, repo.nodes[0].ID)
				if err := c.finalizeRunIfDone(ctx, runID); err != nil {
					t.Fatal(err)
				}
				if fail {
					if !fault.failed {
						t.Fatal("failure not injected")
					}
					if !executionStarted(w.err) {
						t.Fatalf("persistence error not propagated: %v", w.err)
					}
					if len(repo.pendingDelays)+len(repo.pendingWaits) != 0 {
						t.Fatal("unproven pending row")
					}
					if repo.runs[runID].Status != automationdom.RunStatusFailed {
						t.Errorf("write failure finalized as %s", repo.runs[runID].Status)
					}
					if len(repo.runSteps) != 0 {
						t.Fatal("invented step evidence")
					}
				} else if kind == "action" {
					if repo.runs[runID].Status != automationdom.RunStatusCompleted || updater.calls != 2 {
						t.Fatalf("action control status=%s calls=%d", repo.runs[runID].Status, updater.calls)
					}
				} else {
					expected := 1
					if kind == "fanout" {
						expected = 2
					}
					if len(repo.pendingDelays)+len(repo.pendingWaits) != expected || repo.runs[runID].Status != automationdom.RunStatusRunning {
						t.Fatal("lawful pause lost")
					}
				}
				expectedActions := 0
				if kind == "action" {
					expectedActions = 1
					if !fail {
						expectedActions = 2
					}
				}
				if updater.calls != expectedActions {
					t.Fatalf("downstream effects %d want %d", updater.calls, expectedActions)
				}
				if kind == "agent" || kind == "fanout" {
					expected := 1
					if kind == "fanout" {
						expected = 2
					}
					if c.agentMessenger.(*fakeAgentMessenger).taskCalls != expected {
						t.Fatal("already-issued conversations lost or replayed")
					}
				}
			})
		}
	}
}

func (r *oneShotStepFailureRepo) DeletePendingDelay(ctx context.Context, id uuid.UUID) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	return r.fakePauseResumeRepo.DeletePendingDelay(ctx, id)
}

func persistenceDelivery(t *testing.T, c *AutomationConsumer, stream string, values map[string]any) redis.XMessage {
	t.Helper()
	ctx := context.Background()
	if err := c.client.XGroupCreateMkStream(ctx, stream, c.groupName, "0").Err(); err != nil {
		t.Fatal(err)
	}
	if err := c.client.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: values}).Err(); err != nil {
		t.Fatal(err)
	}
	messages, err := c.client.XReadGroup(ctx, &redis.XReadGroupArgs{Group: c.groupName, Consumer: c.consumerName, Streams: []string{stream, ">"}, Count: 1}).Result()
	if err != nil {
		t.Fatal(err)
	}
	return messages[0].Messages[0]
}

func TestAutomationPersistenceFailureConsumption(t *testing.T) {
	for _, entry := range []string{"external", "agent", "delay"} {
		for _, mode := range []string{"step_failure", "state_write_failure", "preflight_refusal", "control"} {
			t.Run(entry+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				c, client, repo, updater, runID, _, task := newPauseResumeFixture(t)
				defer client.Close()
				var logs bytes.Buffer
				c.log = slog.New(slog.NewTextHandler(&logs, nil))
				fault := &oneShotStepFailureRepo{fakePauseResumeRepo: repo}
				stateErr := errors.New("state write unavailable")
				if mode == "state_write_failure" {
					fault.stateErr = stateErr
				}
				if mode != "control" {
					c.repo = fault
				}
				var stream string
				var msg redis.XMessage
				switch entry {
				case "external":
					repo.pendingWaits = nil
					repo.runSteps = nil
					repo.runs = nil
					trigger := repo.nodes[len(repo.nodes)-1]
					trigger.Type = string(automationdom.TriggerAPITrigger)
					trigger.Config, _ = json.Marshal(automationdom.TriggerConfig{TargetTaskID: &task.ID})
					repo.edges = append(repo.edges, &automationdom.Edge{ID: uuid.New(), AutomationID: repo.automation.ID, SourceNodeID: trigger.ID, TargetNodeID: repo.nodes[0].ID})
					c.agentMessenger.(*fakeAgentMessenger).taskCalls = 0
					raw, _ := json.Marshal(automationExternalTriggerPayload{NodeID: trigger.ID.String(), AutomationID: repo.automation.ID.String(), ProjectID: repo.automation.ProjectID.String(), TaskID: task.ID.String()})
					stream = events.StreamAutomationExternalTriggers
					msg = persistenceDelivery(t, c, stream, map[string]any{"payload": string(raw)})
				case "agent":
					stream = events.StreamAgentConversationStatus
					msg = persistenceDelivery(t, c, stream, map[string]any{"conversation_id": conversationIDFromPendingWaits(repo.pendingWaits).String(), "status": "finished"})
				case "delay":
					waitConsumer, waitRepo, waitUpdater, waitRunID := newWaitPauseFixture(t)
					waitConsumer.client = client
					waitConsumer.log = c.log
					c = waitConsumer
					repo = waitRepo
					updater = waitUpdater
					runID = waitRunID
					fault.fakePauseResumeRepo = repo
					if mode != "control" {
						c.repo = fault
					}
					for _, d := range repo.pendingDelays {
						d.ResumeAt = time.Now().Add(-time.Minute)
					}
				}
				if mode == "preflight_refusal" {
					repo.automation.Status = automationdom.StatusInactive
				}
				if entry == "external" {
					c.handleExternalTrigger(msg)
				} else if entry == "agent" {
					c.handleAgentConversationStatus(msg)
				} else {
					scheduler := NewWaitScheduler(client, c, c.log)
					scheduler.tick(ctx)
					scheduler.tick(ctx)
				}
				if mode == "preflight_refusal" {
					if fault.failed || updater.calls != 0 {
						t.Fatal("refusal reached effects or recording")
					}
					if entry == "external" {
						if len(repo.runs) != 0 {
							t.Fatal("refusal created run")
						}
					} else if len(repo.pendingWaits)+len(repo.pendingDelays) != 1 {
						t.Fatal("refusal consumed pending")
					}
				} else {
					if entry == "external" {
						if c.agentMessenger.(*fakeAgentMessenger).taskCalls != 1 || len(repo.runs) != 1 {
							t.Fatal("external dispatch was lost or replayed")
						}
						for id := range repo.runs {
							runID = id
						}
					} else if updater.calls != 1 {
						t.Fatalf("continuation action calls=%d", updater.calls)
					}
					if mode != "control" {
						if !fault.failed || len(repo.pendingWaits)+len(repo.pendingDelays) != 0 {
							t.Fatal("failure not consumed")
						}
						want := automationdom.RunStatusFailed
						if mode == "state_write_failure" {
							want = automationdom.RunStatusRunning
						}
						if repo.runs[runID].Status != want {
							t.Fatalf("outcome=%s want=%s", repo.runs[runID].Status, want)
						}
						if mode == "state_write_failure" && !strings.Contains(logs.String(), stateErr.Error()) {
							t.Fatal("failed state write was concealed")
						}
					}
				}
				if stream != "" {
					pending, err := client.XPending(ctx, stream, c.groupName).Result()
					if err != nil {
						t.Fatal(err)
					}
					want := int64(0)
					if mode == "preflight_refusal" {
						want = 1
					}
					if pending.Count != want {
						t.Fatalf("message pending=%d want=%d", pending.Count, want)
					}
				}
			})
		}
	}
}

func TestAutomationPersistenceFailureAlsoReportsCleanupFailure(t *testing.T) {
	ctx := context.Background()
	c, repo, updater, runID := newWaitPauseFixture(t)
	_, client := newTestConsumerWithRedis(t, repo, c.taskRepo)
	defer client.Close()
	var logs bytes.Buffer
	c.log = slog.New(slog.NewTextHandler(&logs, nil))
	fault := &oneShotStepFailureRepo{fakePauseResumeRepo: repo, deleteErr: errors.New("delay delete unavailable")}
	c.repo = fault
	for _, d := range repo.pendingDelays {
		d.ResumeAt = time.Now().Add(-time.Minute)
	}
	scheduler := NewWaitScheduler(client, c, c.log)
	scheduler.tick(ctx)
	scheduler.tick(ctx)
	if updater.calls != 1 || repo.runs[runID].Status != automationdom.RunStatusFailed || len(repo.pendingDelays) != 1 {
		t.Fatal("failed cleanup replayed action or claimed success")
	}
	if !strings.Contains(logs.String(), fault.deleteErr.Error()) {
		t.Fatal("cleanup failure was concealed")
	}
}
