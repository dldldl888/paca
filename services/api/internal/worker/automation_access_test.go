package worker

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/redis/go-redis/v9"
	"testing"
	"time"

	automationdom "github.com/Paca-AI/api/internal/domain/automation"
	sprintdom "github.com/Paca-AI/api/internal/domain/sprint"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/google/uuid"
)

// Existing engine tests use in-memory rows. Populate the same project/identity
// bounds the real repositories return, and exercise the real grant policy.
func prepareTaskConsumer(c *AutomationConsumer, pid uuid.UUID, task *taskdom.Task) uuid.UUID {
	if task != nil && task.ProjectID != uuid.Nil {
		pid = task.ProjectID
	}
	if pid == uuid.Nil {
		pid = uuid.New()
	}
	r, ok := c.taskRepo.(*fakeAutomationTaskReader)
	if !ok {
		r = &fakeAutomationTaskReader{}
		c.taskRepo = r
	}
	if r.byID == nil {
		r.byID = map[uuid.UUID]*taskdom.Task{}
	}
	seed := func(t *taskdom.Task) {
		if t.ID == uuid.Nil {
			t.ID = uuid.New()
		}
		if t.ProjectID == uuid.Nil {
			t.ProjectID = pid
		}
		r.byID[t.ID] = t
	}
	if task != nil {
		seed(task)
	}
	for _, t := range r.byID {
		seed(t)
	}
	for parent, children := range r.children {
		for _, t := range children {
			if t.ParentTaskID == nil {
				p := parent
				t.ParentTaskID = &p
			}
			seed(t)
		}
	}
	if c.taskSvc == nil {
		c.taskSvc = &fakeTaskUpdater{}
	}
	if f, ok := c.taskSvc.(*fakeTaskUpdater); ok {
		f.reader = r
	}
	return pid
}

func (f *fakeTaskUpdater) CheckTaskAccess(ctx context.Context, pid, id uuid.UUID, action taskdom.AccessAction) (*taskdom.Task, error) {
	if f.reader == nil {
		return nil, taskdom.ErrTaskForbidden
	}
	t, err := f.reader.FindTaskByID(ctx, id)
	if err != nil {
		return nil, err
	}
	scope, err := authz.NewTaskAccessPolicy(nil).ResolveTaskAccess(ctx, pid)
	if err != nil {
		return nil, err
	}
	if err := scope.Require(t, action); err != nil {
		return nil, err
	}
	return t, nil
}

func applyUpdateTaskFixture(c *AutomationConsumer, ctx context.Context, pid uuid.UUID, task *taskdom.Task, upd *automationdom.TaskFieldUpdate, name string, vars map[string]string) (bool, error) {
	pid = prepareTaskConsumer(c, pid, task)
	return c.applyUpdateTask(ctx, pid, task, upd, name, vars)
}
func resolveTargetTasksFixture(c *AutomationConsumer, ctx context.Context, pid uuid.UUID, task *taskdom.Task, target *automationdom.TaskTarget) ([]*taskdom.Task, error) {
	pid = prepareTaskConsumer(c, pid, task)
	return c.resolveTargetTasks(ctx, pid, task, target)
}

func (f *fakePauseResumeRepo) FindRunByID(_ context.Context, id uuid.UUID) (*automationdom.Run, error) {
	if run := f.runs[id]; run != nil {
		return run, nil
	}
	return nil, automationdom.ErrRunNotFound
}

// This fixture validator checks current active/project/node linkage. Real graph
// config validation and the SQL project predicate are tested by enabled E2E.
type fixtureExecutionValidator struct{ repo automationGraphReader }

func (v fixtureExecutionValidator) ValidateExecution(ctx context.Context, pid, nodeID uuid.UUID) (*automationdom.Graph, error) {
	a, err := v.repo.FindAutomationByNodeID(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if a == nil || a.ProjectID != pid || a.Status != automationdom.StatusActive {
		return nil, automationdom.ErrNotFound
	}
	g, err := v.repo.LoadGraph(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	for _, n := range g.Nodes {
		if n.ID == nodeID && n.AutomationID == a.ID {
			return g, nil
		}
	}
	return nil, automationdom.ErrNodeNotFound
}

func prepareWalkerFixture(w *walker) {
	c := w.consumer
	w.projectID = prepareTaskConsumer(c, w.projectID, w.task)
	if w.sprint != nil {
		w.sprint.ProjectID = w.projectID
		prepareSprintFixture(c, w.sprint)
	}
	if m, ok := c.memberRepo.(*fakeAutomationMemberReader); ok && m.member != nil {
		m.member.ProjectID = w.projectID
	}
	r, ok := c.repo.(*fakePauseResumeRepo)
	if !ok {
		return
	}
	r.automation.ProjectID = w.projectID
	if r.automation.GraphRevision == 0 {
		r.automation.GraphRevision = 1
	}
	trigger := &automationdom.Node{ID: uuid.New(), AutomationID: r.automation.ID, Kind: automationdom.KindTrigger, Type: string(automationdom.TriggerTaskCreated), Config: json.RawMessage(`{}`)}
	if w.sprint != nil {
		trigger.Type = string(automationdom.TriggerSprintStarted)
	}
	r.nodes = append(r.nodes, trigger)
	for _, run := range r.runs {
		run.GraphRevision = r.automation.GraphRevision
		run.SprintID = nil
		if w.sprint != nil {
			run.SprintID = &w.sprint.ID
		}
		run.TaskID = nil
		if w.task != nil {
			run.TaskID = &w.task.ID
		}
		run.TriggerNodeID = trigger.ID
		run.StartedAt = time.Now()
	}
	c.WithExecutionValidator(fixtureExecutionValidator{r})
}

func TestAutomationTargetsRejectForeignAndQueryFailureBeforeFanout(t *testing.T) {
	pid := uuid.New()
	base := &taskdom.Task{ID: uuid.New(), ProjectID: pid}
	first := &taskdom.Task{ID: uuid.New(), ProjectID: pid, ParentTaskID: &base.ID}
	bad := &taskdom.Task{ID: uuid.New(), ProjectID: uuid.New(), ParentTaskID: &base.ID}
	updater := &fakeTaskUpdater{}
	c := &AutomationConsumer{taskSvc: updater, taskRepo: &fakeAutomationTaskReader{children: map[uuid.UUID][]*taskdom.Task{base.ID: {first, bad}}}}
	prepareTaskConsumer(c, pid, base)
	cfg, _ := json.Marshal(automationdom.ActionConfig{Target: &automationdom.TaskTarget{Kind: automationdom.TaskTargetChildren}, Update: &automationdom.TaskFieldUpdate{Title: "forbidden fanout"}})
	_, _, err := c.runAction(context.Background(), pid, &automationdom.Node{Type: string(automationdom.ActionUpdateTask), Config: cfg}, base, nil, nil, "test", uuid.New())
	if err == nil || updater.calls != 0 {
		t.Fatalf("foreign late target: err=%v writes=%d", err, updater.calls)
	}
}

func runActionFixture(c *AutomationConsumer, ctx context.Context, pid uuid.UUID, node *automationdom.Node, task *taskdom.Task, sprint *sprintdom.Sprint, vars map[string]string, name string, runID uuid.UUID) (bool, json.RawMessage, error) {
	pid = prepareTaskConsumer(c, pid, task)
	return c.runAction(ctx, pid, node, task, sprint, vars, name, runID)
}
func triggerAgentFixture(c *AutomationConsumer, ctx context.Context, pid uuid.UUID, task *taskdom.Task, memberID uuid.UUID, name, message string) (bool, *uuid.UUID, error) {
	pid = prepareTaskConsumer(c, pid, task)
	if m, ok := c.memberRepo.(*fakeAutomationMemberReader); ok && m.member != nil {
		m.member.ProjectID = pid
	}
	return c.applyTriggerAIAgentOnTask(ctx, pid, task, memberID, name, message)
}
func directAgentFixture(c *AutomationConsumer, ctx context.Context, pid, memberID uuid.UUID, message string) (bool, *uuid.UUID, error) {
	if m, ok := c.memberRepo.(*fakeAutomationMemberReader); ok && m.member != nil {
		m.member.ProjectID = pid
	}
	return c.applyDirectAgentMessage(ctx, pid, memberID, message)
}

func TestPluginAutomationDeniesBeforeWASM(t *testing.T) {
	c := newTestConsumer(&fakeTaskUpdater{})
	node := &automationdom.Node{ID: uuid.New(), Type: "com.acme.raw_task_db", Config: json.RawMessage(`{"target":{"kind":"other"}}`)}
	if applied, err := c.runPluginAction(context.Background(), uuid.New(), node, &taskdom.Task{ID: uuid.New()}, uuid.New()); err == nil || applied {
		t.Fatalf("action %v %v", applied, err)
	}
	if matched, err := c.evaluatePluginCondition(context.Background(), node, []byte(`{}`)); err == nil || matched {
		t.Fatalf("condition %v %v", matched, err)
	}
	for _, mode := range []string{"any", "all"} {
		if matched, _, err := c.evaluatePluginConditionAgainstTasks(context.Background(), uuid.New(), node, []*taskdom.Task{{ID: uuid.New()}}, mode); err == nil || matched {
			t.Fatalf("fanout condition %v %v", matched, err)
		}
	}
}

func (f *fakeAutomationTaskReader) ListOpenTaskIDsInSprint(_ context.Context, pid, sid uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	for _, task := range f.byID {
		if task.ProjectID == pid && task.SprintID != nil && *task.SprintID == sid {
			ids = append(ids, task.ID)
		}
	}
	return ids, nil
}
func prepareSprintFixture(c *AutomationConsumer, sp *sprintdom.Sprint) uuid.UUID {
	pid := sp.ProjectID
	if pid == uuid.Nil {
		pid = uuid.New()
		sp.ProjectID = pid
	}
	task := &taskdom.Task{ID: uuid.New(), ProjectID: pid, SprintID: &sp.ID}
	prepareTaskConsumer(c, pid, task)
	return pid
}

func (f *fakeAutomationTaskReader) ListChildTaskIDs(_ context.Context, parent uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	for _, t := range f.children[parent] {
		ids = append(ids, t.ID)
	}
	return ids, nil
}

func TestAutomationEntryRevalidatesBeforeRun(t *testing.T) {
	for _, mode := range []string{"missing_validator", "wrong_project", "stale_config", "inactive", "foreign_child", "valid"} {
		t.Run(mode, func(t *testing.T) {
			pid := uuid.New()
			task := &taskdom.Task{ID: uuid.New(), ProjectID: pid}
			aid := uuid.New()
			cfg, _ := json.Marshal(automationdom.TriggerConfig{TargetTaskID: &task.ID})
			trigger := &automationdom.Node{ID: uuid.New(), AutomationID: aid, Kind: automationdom.KindTrigger, Type: string(automationdom.TriggerAPITrigger), Config: cfg}
			action := &automationdom.Node{ID: uuid.New(), AutomationID: aid, Kind: automationdom.KindAction, Type: string(automationdom.ActionUpdateTask), Config: json.RawMessage(`{"update":{"title":"updated"}}`)}
			repo := &fakePauseResumeRepo{automation: &automationdom.Automation{ID: aid, ProjectID: pid, Status: automationdom.StatusActive}, nodes: []*automationdom.Node{trigger, action}, edges: []*automationdom.Edge{{AutomationID: aid, SourceNodeID: trigger.ID, TargetNodeID: action.ID}}}
			updater := &fakeTaskUpdater{}
			c := &AutomationConsumer{repo: repo, taskSvc: updater, log: discardLogger()}
			prepareTaskConsumer(c, pid, task)
			c.WithExecutionValidator(fixtureExecutionValidator{repo})
			hint := *trigger
			switch mode {
			case "missing_validator":
				c.executionValidator = nil
			case "wrong_project":
				pid = uuid.New()
			case "stale_config":
				trigger.Config = json.RawMessage(`{}`)
			case "inactive":
				repo.automation.Status = automationdom.StatusInactive
			case "foreign_child":
				bad := &taskdom.Task{ID: uuid.New(), ProjectID: uuid.New(), ParentTaskID: &task.ID}
				r := c.taskRepo.(*fakeAutomationTaskReader)
				r.byID[bad.ID] = bad
				r.children = map[uuid.UUID][]*taskdom.Task{task.ID: {bad}}
				action.Config = json.RawMessage(`{"target":{"kind":"children"},"update":{"title":"updated"}}`)
			}
			err := c.runGraphWalk(context.Background(), pid, repo.automation, &hint, task, nil)
			if mode == "valid" {
				if err != nil || len(repo.runs) != 1 || updater.calls != 1 {
					t.Fatalf("valid run: %v runs=%d writes=%d", err, len(repo.runs), updater.calls)
				}
				return
			}
			if err == nil || len(repo.runs) != 0 || len(repo.runSteps) != 0 || updater.calls != 0 {
				t.Fatalf("refusal: %v runs=%d steps=%d writes=%d", err, len(repo.runs), len(repo.runSteps), updater.calls)
			}
		})
	}
}

func TestAutomationResumeRejectsDisabledChangedAndForgedContext(t *testing.T) {
	for _, mode := range []string{"disabled", "changed_node", "changed_project", "changed_task", "wrong_run", "wrong_node"} {
		t.Run(mode, func(t *testing.T) {
			c, repo, updater, runID := newWaitPauseFixture(t)
			var delay *automationdom.PendingDelay
			for _, d := range repo.pendingDelays {
				delay = d
			}
			before := len(repo.runSteps)
			switch mode {
			case "disabled":
				repo.automation.Status = automationdom.StatusInactive
			case "changed_node":
				repo.nodes[0].UpdatedAt = time.Now().Add(time.Second)
				repo.automation.GraphRevision++ // actual repository mutation increments this atomically
			case "changed_project":
				delay.ProjectID = uuid.New()
			case "changed_task":
				id := uuid.New()
				delay.Context.TaskID = &id
			case "wrong_run":
				delay.RunID = uuid.New()
			case "wrong_node":
				delay.NodeID = uuid.New()
			}
			if err := c.resumeAfterDelay(context.Background(), delay); err == nil {
				t.Fatal("invalid resume accepted")
			}
			if updater.calls != 0 || len(repo.runSteps) != before || len(repo.pendingDelays) != 1 || repo.runs[runID].Status != automationdom.RunStatusRunning {
				t.Fatal("refused resume had effects")
			}
		})
	}
}

func TestAutomationWebhookQueueUsesCurrentTarget(t *testing.T) {
	pid, aid := uuid.New(), uuid.New()
	task := &taskdom.Task{ID: uuid.New(), ProjectID: pid}
	cfg, _ := json.Marshal(automationdom.TriggerConfig{TargetTaskID: &task.ID})
	trigger := &automationdom.Node{ID: uuid.New(), AutomationID: aid, Kind: automationdom.KindTrigger, Type: string(automationdom.TriggerAPITrigger), Config: cfg}
	repo := &fakePauseResumeRepo{automation: &automationdom.Automation{ID: aid, ProjectID: pid, Status: automationdom.StatusActive}, nodes: []*automationdom.Node{trigger}}
	updater := &fakeTaskUpdater{}
	c, client := newTestConsumerWithRedis(t, repo, &fakeAutomationTaskReader{})
	defer client.Close()
	c.taskSvc = updater
	prepareTaskConsumer(c, pid, task)
	c.WithExecutionValidator(fixtureExecutionValidator{repo})
	for _, mode := range []string{"old_target", "forged_automation", "wrong_project"} {
		payload := automationExternalTriggerPayload{NodeID: trigger.ID.String(), AutomationID: aid.String(), ProjectID: pid.String(), TaskID: task.ID.String()}
		switch mode {
		case "old_target":
			payload.TaskID = uuid.NewString()
		case "forged_automation":
			payload.AutomationID = uuid.NewString()
		case "wrong_project":
			payload.ProjectID = uuid.NewString()
		}
		raw, _ := json.Marshal(payload)
		c.handleExternalTrigger(redis.XMessage{ID: "1-1", Values: map[string]any{"payload": string(raw)}})
		if len(repo.runs) != 0 || updater.calls != 0 {
			t.Fatalf("%s created run/write", mode)
		}
	}
}

type failingAutomationLinks struct {
	automationTaskReader
	err error
}

func (f failingAutomationLinks) ListTaskLinks(context.Context, uuid.UUID) ([]*taskdom.TaskLink, error) {
	return nil, f.err
}
func TestAutomationTargetQueryErrorPropagates(t *testing.T) {
	pid := uuid.New()
	base := &taskdom.Task{ID: uuid.New(), ProjectID: pid}
	c := newTestConsumer(&fakeTaskUpdater{})
	prepareTaskConsumer(c, pid, base)
	failure := errors.New("link database unavailable")
	c.taskRepo = failingAutomationLinks{c.taskRepo, failure}
	if _, err := c.resolveTargetTasks(context.Background(), pid, base, &automationdom.TaskTarget{Kind: automationdom.TaskTargetBlocks}); !errors.Is(err, failure) {
		t.Fatalf("query error became empty target: %v", err)
	}
}

func TestAutomationForeignParentOtherAndLinkDenied(t *testing.T) {
	for _, kind := range []automationdom.TaskTargetKind{automationdom.TaskTargetParent, automationdom.TaskTargetOther, automationdom.TaskTargetBlocks} {
		t.Run(string(kind), func(t *testing.T) {
			pid := uuid.New()
			base := &taskdom.Task{ID: uuid.New(), ProjectID: pid}
			foreign := &taskdom.Task{ID: uuid.New(), ProjectID: uuid.New()}
			repo := &fakeAutomationTaskReader{byID: map[uuid.UUID]*taskdom.Task{base.ID: base, foreign.ID: foreign}}
			target := &automationdom.TaskTarget{Kind: kind}
			switch kind {
			case automationdom.TaskTargetParent:
				base.ParentTaskID = &foreign.ID
			case automationdom.TaskTargetOther:
				target.OtherTaskID = &foreign.ID
			case automationdom.TaskTargetBlocks:
				repo.links = map[uuid.UUID][]*taskdom.TaskLink{base.ID: {{SourceTaskID: base.ID, TargetTaskID: foreign.ID, DisplayLinkType: "blocks"}}}
			}
			updater := &fakeTaskUpdater{}
			c := &AutomationConsumer{taskRepo: repo, taskSvc: updater}
			prepareTaskConsumer(c, pid, base)
			cfg, _ := json.Marshal(automationdom.ActionConfig{Target: target, Update: &automationdom.TaskFieldUpdate{Title: "forbidden"}})
			if _, _, err := c.runAction(context.Background(), pid, &automationdom.Node{Type: string(automationdom.ActionUpdateTask), Config: cfg}, base, nil, nil, "test", uuid.New()); err == nil || updater.calls != 0 {
				t.Fatalf("foreign %s err=%v writes=%d", kind, err, updater.calls)
			}
		})
	}
}
