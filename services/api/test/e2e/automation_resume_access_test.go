package e2e_test

import (
	"context"
	"encoding/json"
	automationdom "github.com/Paca-AI/api/internal/domain/automation"
	sprintdom "github.com/Paca-AI/api/internal/domain/sprint"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/worker"
	"github.com/google/uuid"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Real worker/repository/scheduler regression: the unchanged control run proves
// the scheduler actually processed due delays before negative state is asserted.
func TestAutomationResumeDurableBindings(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "resume-manager", "password1")
	client, token := taskMemberLogin(t, env, "resume-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	user, err := env.userRepo.FindByUsername(env.ctx, "resume-manager")
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithActorID(env.ctx, user.ID)
	consumer := startAutomationConsumer(t, env)
	sprint, err := env.sprintSvc.CreateSprint(ctx, sprintdom.CreateSprintInput{ProjectID: pid, Name: "unrelated"})
	if err != nil {
		t.Fatal(err)
	}
	type paused struct {
		automation, task, wait, alternate, action, edge uuid.UUID
		delay                                           *automationdom.PendingDelay
	}
	cases := map[string]*paused{}
	for _, name := range []string{"edge_deleted", "node_deleted", "other_wait", "task_sprint", "pending_id", "legacy", "missing_step", "control"} {
		task, err := env.taskSvc.CreateTask(ctx, taskdom.CreateTaskInput{ProjectID: pid, Title: name})
		if err != nil {
			t.Fatal(err)
		}
		a, err := env.automationSvc.CreateAutomation(ctx, automationdom.CreateAutomationInput{ProjectID: pid, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		add := func(kind automationdom.Kind, typ string, cfg any) *automationdom.Node {
			t.Helper()
			raw, _ := json.Marshal(cfg)
			n, err := env.automationSvc.AddNode(ctx, pid, a.ID, automationdom.AddNodeInput{Kind: kind, Type: typ, Config: raw})
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
		tr := add(automationdom.KindTrigger, "api_trigger", map[string]any{"target_task_id": task.ID})
		wait := add(automationdom.KindAction, "wait", map[string]any{"wait_minutes": 1})
		alternate := add(automationdom.KindAction, "wait", map[string]any{"wait_minutes": 1})
		action := add(automationdom.KindAction, "update_task", map[string]any{"update": map[string]any{"title": "resumed"}})
		edge := func(from, to uuid.UUID) *automationdom.Edge {
			t.Helper()
			e, err := env.automationSvc.AddEdge(ctx, pid, a.ID, automationdom.AddEdgeInput{SourceNodeID: from, TargetNodeID: to})
			if err != nil {
				t.Fatal(err)
			}
			return e
		}
		edge(tr.ID, wait.ID)
		out := edge(wait.ID, action.ID)
		edge(alternate.ID, action.ID)
		if _, err := env.automationSvc.Activate(ctx, pid, a.ID); err != nil {
			t.Fatal(err)
		}
		_, secret, err := env.automationSvc.GenerateWebhookToken(ctx, pid, a.ID, tr.ID, automationdom.WebhookTokenActor{})
		if err != nil {
			t.Fatal(err)
		}
		if err := env.automationSvc.HandleWebhookTrigger(env.ctx, tr.ID, secret); err != nil {
			t.Fatal(err)
		}
		var delayID uuid.UUID
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			err = env.db.Get(&delayID, `SELECT id FROM automation_pending_delays WHERE automation_id=$1`, a.ID)
			if err == nil {
				break
			}
			time.Sleep(30 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(`UPDATE automation_pending_delays SET resume_at=NOW()-INTERVAL '1 second' WHERE id=$1`, delayID); err != nil {
			t.Fatal(err)
		}
		delays, err := env.automationRepo.ListDueDelays(env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		var delay *automationdom.PendingDelay
		for _, d := range delays {
			if d.ID == delayID {
				delay = d
			}
		}
		if delay == nil {
			t.Fatal("pending row not reloaded")
		}
		cases[name] = &paused{a.ID, task.ID, wait.ID, alternate.ID, action.ID, out.ID, delay}
	}
	if err := env.automationSvc.RemoveEdge(ctx, pid, cases["edge_deleted"].automation, cases["edge_deleted"].edge); err != nil {
		t.Fatal(err)
	}
	if err := env.automationSvc.RemoveNode(ctx, pid, cases["node_deleted"].automation, cases["node_deleted"].alternate); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE automation_pending_delays SET node_id=$1 WHERE id=$2`, cases["other_wait"].alternate, cases["other_wait"].delay.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE automation_pending_delays SET context=jsonb_set(context,'{sprint_id}',to_jsonb($1::text)) WHERE id=$2`, sprint.ID.String(), cases["task_sprint"].delay.ID); err != nil {
		t.Fatal(err)
	}
	forgedID := uuid.New()
	if _, err := env.db.Exec(`UPDATE automation_pending_delays SET id=$1 WHERE id=$2`, forgedID, cases["pending_id"].delay.ID); err != nil {
		t.Fatal(err)
	}
	cases["pending_id"].delay.ID = forgedID
	if _, err := env.db.Exec(`UPDATE automation_runs SET graph_revision=0 WHERE id=$1`, cases["legacy"].delay.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE automation_run_steps SET output_snapshot='{}' WHERE run_id=$1`, cases["missing_step"].delay.RunID); err != nil {
		t.Fatal(err)
	}

	scheduler := worker.NewWaitScheduler(env.redisClient, consumer, slog.New(slog.NewTextHandler(io.Discard, nil))).WithInterval(30 * time.Millisecond).WithLeaderKey("e2e.resume." + uuid.NewString())
	scheduler.Start(env.ctx)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, err := env.automationRepo.FindRunByID(env.ctx, cases["control"].delay.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status == automationdom.RunStatusCompleted {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	scheduler.Stop() // joins the complete tick, including rows reordered by pending updates
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			var pending int
			if err := env.db.Get(&pending, `SELECT count(*) FROM automation_pending_delays WHERE id=$1`, p.delay.ID); err != nil {
				t.Fatal(err)
			}
			r, err := env.automationRepo.FindRunByID(env.ctx, p.delay.RunID)
			if err != nil {
				t.Fatal(err)
			}
			task, err := env.taskRepo.FindTaskByID(context.Background(), p.task)
			if err != nil {
				t.Fatal(err)
			}
			if name == "control" {
				if pending != 0 || r.Status != automationdom.RunStatusCompleted || task.Title != "resumed" {
					t.Fatalf("lawful resume pending=%d status=%s title=%s", pending, r.Status, task.Title)
				}
				return
			}
			if pending != 1 || r.Status != automationdom.RunStatusRunning || task.Title != name {
				t.Fatalf("invalid resume had effects pending=%d status=%s title=%s", pending, r.Status, task.Title)
			}
		})
	}
}

func TestAutomationGraphSnapshotRevision(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "revision-manager", "password1")
	client, token := taskMemberLogin(t, env, "revision-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	user, err := env.userRepo.FindByUsername(env.ctx, "revision-manager")
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithActorID(env.ctx, user.ID)
	a, err := env.automationSvc.CreateAutomation(ctx, automationdom.CreateAutomationInput{ProjectID: pid, Name: "revision"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := env.automationSvc.AddNode(ctx, pid, a.ID, automationdom.AddNodeInput{Kind: automationdom.KindAction, Type: "wait", Config: json.RawMessage(`{"wait_minutes":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	before, err := env.automationRepo.LoadGraph(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Block the graph's node SELECT after its metadata/revision SELECT has taken
	// its snapshot; commit an edit between those two reads on another connection.
	tx, err := env.db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`LOCK TABLE automation_nodes IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	type result struct {
		g   *automationdom.Graph
		err error
	}
	loaded := make(chan result, 1)
	go func() { g, err := env.automationRepo.LoadGraph(ctx, a.ID); loaded <- result{g, err} }()
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		if err := env.db.Get(&blocked, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT id,automation_id,kind,type,config%')`); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("graph reader never reached blocked node SELECT")
	}
	if _, err := tx.Exec(`UPDATE automation_nodes SET config='{"wait_minutes":2}' WHERE id=$1`, node.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var old *automationdom.Graph
	select {
	case r := <-loaded:
		if r.err != nil {
			t.Fatal(r.err)
		}
		old = r.g
	case <-time.After(5 * time.Second):
		t.Fatal("graph reader did not finish")
	}
	current, err := env.automationRepo.LoadGraph(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var cfg automationdom.ActionConfig
	if err := json.Unmarshal(old.Nodes[0].Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if old.Automation.GraphRevision != before.Automation.GraphRevision || cfg.WaitMinutes == nil || *cfg.WaitMinutes != 1 || current.Automation.GraphRevision != old.Automation.GraphRevision+1 {
		t.Fatalf("mixed snapshot: old=%d before=%d current=%d config=%s", old.Automation.GraphRevision, before.Automation.GraphRevision, current.Automation.GraphRevision, old.Nodes[0].Config)
	}
	run := &automationdom.Run{ID: uuid.New(), AutomationID: a.ID, TriggerNodeID: node.ID, GraphRevision: old.Automation.GraphRevision, Status: automationdom.RunStatusRunning, StartedAt: time.Now()}
	if err := env.automationRepo.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	saved, err := env.automationRepo.FindRunByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.GraphRevision != old.Automation.GraphRevision || saved.GraphRevision == current.Automation.GraphRevision {
		t.Fatal("run acquired a later revision than its snapshot")
	}
	// All actual mutation forms, including cascade, atomically advance revision.
	previous := current.Automation.GraphRevision
	changed := func() {
		t.Helper()
		g, err := env.automationRepo.LoadGraph(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if g.Automation.GraphRevision <= previous {
			t.Fatal("mutation did not advance revision")
		}
		previous = g.Automation.GraphRevision
	}
	n2 := &automationdom.Node{ID: uuid.New(), AutomationID: a.ID, Kind: automationdom.KindAction, Type: "wait", Config: json.RawMessage(`{"wait_minutes":1}`), CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := env.automationRepo.CreateNode(ctx, n2); err != nil {
		t.Fatal(err)
	}
	changed()
	edge := &automationdom.Edge{ID: uuid.New(), AutomationID: a.ID, SourceNodeID: node.ID, TargetNodeID: n2.ID, CreatedAt: time.Now()}
	if err := env.automationRepo.CreateEdge(ctx, edge); err != nil {
		t.Fatal(err)
	}
	changed()
	if err := env.automationRepo.DeleteEdge(ctx, edge.ID); err != nil {
		t.Fatal(err)
	}
	changed()
	if err := env.automationRepo.CreateEdge(ctx, edge); err != nil {
		t.Fatal(err)
	}
	changed()
	if err := env.automationRepo.DeleteNode(ctx, n2.ID); err != nil {
		t.Fatal(err)
	}
	changed()
	var count int
	if err := env.db.Get(&count, `SELECT count(*) FROM automation_edges WHERE id=$1`, edge.ID); err != nil || count != 0 {
		t.Fatalf("cascade %d %v", count, err)
	}
	name := "new metadata"
	if _, err := env.automationSvc.UpdateAutomation(ctx, pid, a.ID, automationdom.UpdateAutomationInput{Name: &name}); err != nil {
		t.Fatal(err)
	}
	changed()
	tx, err = env.db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM automation_nodes WHERE id=$1`, node.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	final, err := env.automationRepo.LoadGraph(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Automation.GraphRevision != previous || len(final.Nodes) != 1 {
		t.Fatal("rolled-back mutation or revision escaped transaction")
	}
}

func TestAutomationSprintResumeOriginalBinding(t *testing.T) {
	env := newE2EEnv(t)
	consumer := startAutomationConsumer(t, env)
	seedTaskMemberUser(t, env, "sprint-resume-manager", "password1")
	client, token := taskMemberLogin(t, env, "sprint-resume-manager", "password1")
	pid := createProjectForTasksViaAPI(t, env, client, token)
	sidA := createSprintViaAPI(t, env, client, token, pid, "A")
	sidB := createSprintViaAPI(t, env, client, token, pid, "B")
	aid := createAutomationViaAPI(t, env, client, token, pid, "sprint resume")
	trigger := addAutomationNodeViaAPI(t, env, client, token, pid, aid, "trigger", "sprint_started", nil)
	wait := addAutomationNodeViaAPI(t, env, client, token, pid, aid, "action", "wait", map[string]any{"wait_minutes": 1})
	action := addAutomationNodeViaAPI(t, env, client, token, pid, aid, "action", "update_sprint", map[string]any{"sprint_update": map[string]any{"name": "resumed"}})
	addAutomationEdgeViaAPI(t, env, client, token, pid, aid, idOf(trigger), idOf(wait))
	addAutomationEdgeViaAPI(t, env, client, token, pid, aid, idOf(wait), idOf(action))
	activateAutomationViaAPI(t, env, client, token, pid, aid)
	patchSprintFieldsViaAPI(t, env, client, token, pid, sidA, map[string]any{"status": "active"})
	patchSprintFieldsViaAPI(t, env, client, token, pid, sidB, map[string]any{"status": "active"})
	deadline := time.Now().Add(10 * time.Second)
	var count int
	for time.Now().Before(deadline) {
		if err := env.db.Get(&count, `SELECT count(*) FROM automation_pending_delays WHERE automation_id=$1`, aid); err != nil {
			t.Fatal(err)
		}
		if count == 2 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if count != 2 {
		t.Fatal("two sprint runs did not pause")
	}
	var delayA, runA uuid.UUID
	if err := env.db.QueryRow(`SELECT d.id,r.id FROM automation_pending_delays d JOIN automation_runs r ON r.id=d.run_id WHERE r.automation_id=$1 AND r.sprint_id=$2`, aid, sidA).Scan(&delayA, &runA); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE automation_pending_delays SET context=jsonb_set(context,'{sprint_id}',to_jsonb($1::text)) WHERE id=$2`, sidB, delayA); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE automation_pending_delays SET resume_at=NOW()-INTERVAL '1 second' WHERE automation_id=$1`, aid); err != nil {
		t.Fatal(err)
	}
	scheduler := worker.NewWaitScheduler(env.redisClient, consumer, slog.New(slog.NewTextHandler(io.Discard, nil))).WithInterval(30 * time.Millisecond).WithLeaderKey("e2e.sprint-resume." + uuid.NewString())
	scheduler.Start(env.ctx)
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := env.db.Get(&count, `SELECT count(*) FROM automation_runs WHERE automation_id=$1 AND status='completed'`, aid); err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	scheduler.Stop()
	if count != 1 {
		t.Fatal("lawful sprint resume failed")
	}
	run, err := env.automationRepo.FindRunByID(env.ctx, runA)
	if err != nil {
		t.Fatal(err)
	}
	if run.SprintID == nil || run.SprintID.String() != sidA || run.TaskID != nil || run.Status != automationdom.RunStatusRunning {
		t.Fatalf("original sprint run lost binding: %+v", run)
	}
	if err := env.db.Get(&count, `SELECT count(*) FROM automation_pending_delays WHERE id=$1`, delayA); err != nil || count != 1 {
		t.Fatalf("forged sprint pending lost: %d %v", count, err)
	}
	a, err := env.sprintRepo.FindSprintByID(env.ctx, uuid.MustParse(sidA))
	if err != nil {
		t.Fatal(err)
	}
	b, err := env.sprintRepo.FindSprintByID(env.ctx, uuid.MustParse(sidB))
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "A" || b.Name != "resumed" {
		t.Fatalf("wrong sprint effect A=%s B=%s", a.Name, b.Name)
	}
}

// Observing the consumer's error ensures each refusal completed before assertions.
type resumeErrorSignal struct{ denied chan struct{} }

func (s resumeErrorSignal) Write(p []byte) (int, error) {
	if strings.Contains(string(p), `msg="automation consumer: resume walk"`) {
		select {
		case s.denied <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

func TestAutomationAgentWaitIdentityAndFanout(t *testing.T) {
	env := newE2EEnv(t)
	signal := resumeErrorSignal{make(chan struct{}, 10)}
	consumer := worker.NewAutomationConsumer(env.redisClient, env.automationRepo, env.taskRepo, env.taskSvc, env.activitySvc, nil, slog.New(slog.NewTextHandler(signal, nil))).WithExecutionValidator(env.automationSvc).WithAgentMessaging(env.projectRepo, env.agentSvc).WithConsumerGroup("e2e.agent-binding." + uuid.NewString())
	consumer.Start(env.ctx)
	defer consumer.Stop()
	seedTaskMemberUser(t, env, "agent-resume-manager", "password1")
	client, token := taskMemberLogin(t, env, "agent-resume-manager", "password1")
	pid := createProjectForTasksViaAPI(t, env, client, token)
	roleID := projectRoleIDByName(t, env, client, token, pid, "Editor")
	_, created := createAgentRequest(t, env, client, token, pid, llmAgentBody(roleID, "binding-agent", nil))
	agentID := idOf(assertDataMap(t, created))
	memberID := memberIDForAgent(listProjectMembersViaAPI(t, env, client, token, pid), agentID)
	base := createTaskViaAPI(t, env, client, token, pid, "base")
	for _, name := range []string{"childA", "childB"} {
		createTaskViaAPIWithBody(t, env, client, token, pid, map[string]any{"title": name, "parent_task_id": base})
	}
	aid := createAutomationViaAPI(t, env, client, token, pid, "agent fanout binding")
	tr := addAutomationNodeViaAPI(t, env, client, token, pid, aid, "trigger", "api_trigger", map[string]any{"target_task_id": base})
	agent := addAutomationNodeViaAPI(t, env, client, token, pid, aid, "action", "trigger_ai_agent", map[string]any{"member_id": memberID, "target": map[string]any{"kind": "children"}})
	alternate := addAutomationNodeViaAPI(t, env, client, token, pid, aid, "action", "trigger_ai_agent", map[string]any{"member_id": memberID})
	action := addAutomationNodeViaAPI(t, env, client, token, pid, aid, "action", "update_task", map[string]any{"update": map[string]any{"title": "resumed"}})
	addAutomationEdgeViaAPI(t, env, client, token, pid, aid, idOf(tr), idOf(agent))
	addAutomationEdgeViaAPI(t, env, client, token, pid, aid, idOf(agent), idOf(action))
	addAutomationEdgeViaAPI(t, env, client, token, pid, aid, idOf(alternate), idOf(action))
	activateAutomationViaAPI(t, env, client, token, pid, aid)
	secret := generateWebhookTokenViaAPI(t, env, client, token, pid, aid, idOf(tr))
	postWebhookExpect(t, env, idOf(tr), secret, 202)
	deadline := time.Now().Add(15 * time.Second)
	var convIDs []uuid.UUID
	for time.Now().Before(deadline) {
		if err := env.db.Select(&convIDs, `SELECT conversation_id FROM automation_pending_agent_waits WHERE automation_id=$1 ORDER BY conversation_id`, aid); err != nil {
			t.Fatal(err)
		}
		if len(convIDs) == 2 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if len(convIDs) != 2 {
		t.Fatal("actual agent fanout did not create two waits")
	}
	first, err := env.automationRepo.FindPendingAgentWait(env.ctx, convIDs[0])
	if err != nil || first == nil {
		t.Fatalf("pending %v", err)
	}
	for _, mode := range []string{"pending_id", "node_id", "conversation_id"} {
		t.Run(mode, func(t *testing.T) {
			id := first.ID
			conv := first.ConversationID
			node := first.NodeID
			switch mode {
			case "pending_id":
				id = uuid.New()
			case "node_id":
				node = uuid.MustParse(idOf(alternate))
			case "conversation_id":
				// Use the second real conversation by swapping both pending conversation keys.
				if _, err := env.db.Exec(`DELETE FROM automation_pending_agent_waits WHERE conversation_id=$1`, convIDs[1]); err != nil {
					t.Fatal(err)
				}
				conv = convIDs[1]
			}
			if _, err := env.db.Exec(`UPDATE automation_pending_agent_waits SET id=$1,node_id=$2,conversation_id=$3 WHERE id=$4`, id, node, conv, first.ID); err != nil {
				t.Fatal(err)
			}
			publishAgentConversationStatusViaRedis(t, env, conv.String(), "finished")
			select {
			case <-signal.denied:
			case <-time.After(5 * time.Second):
				t.Fatal("consumer did not report refusal")
			}
			run, err := env.automationRepo.FindRunByID(env.ctx, first.RunID)
			if err != nil {
				t.Fatal(err)
			}
			task, err := env.taskRepo.FindTaskByID(env.ctx, uuid.MustParse(base))
			if err != nil {
				t.Fatal(err)
			}
			var count int
			if err := env.db.Get(&count, `SELECT count(*) FROM automation_pending_agent_waits WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if count != 1 || run.Status != automationdom.RunStatusRunning || task.Title != "base" {
				t.Fatalf("forged %s effects count=%d run=%s task=%s", mode, count, run.Status, task.Title)
			}
			if _, err := env.db.Exec(`UPDATE automation_pending_agent_waits SET id=$1,node_id=$2,conversation_id=$3 WHERE id=$4`, first.ID, first.NodeID, first.ConversationID, id); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Restore second pending from its independently recorded completed-step evidence.
	steps, err := env.automationRepo.ListRunStepsByRun(env.ctx, first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var secondID uuid.UUID
	for _, step := range steps {
		var out struct {
			Pauses []struct {
				ID             uuid.UUID `json:"pending_id"`
				ConversationID uuid.UUID `json:"conversation_id"`
			} `json:"pauses"`
		}
		if json.Unmarshal(step.OutputSnapshot, &out) == nil {
			for _, p := range out.Pauses {
				if p.ConversationID == convIDs[1] {
					secondID = p.ID
				}
			}
		}
	}
	if secondID == uuid.Nil {
		t.Fatal("second durable pending evidence missing")
	}
	second := *first
	second.ID = secondID
	second.ConversationID = convIDs[1]
	if err := env.automationRepo.CreatePendingAgentWait(env.ctx, &second); err != nil {
		t.Fatal(err)
	}
	publishAgentConversationStatusViaRedis(t, env, first.ConversationID.String(), "finished")
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := env.automationRepo.FindPendingAgentWait(env.ctx, first.ConversationID)
		if err != nil {
			t.Fatal(err)
		}
		if pending == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	task, err := env.taskRepo.FindTaskByID(env.ctx, uuid.MustParse(base))
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "base" {
		t.Fatal("first fanout completion ran downstream early")
	}
	publishAgentConversationStatusViaRedis(t, env, convIDs[1].String(), "finished")
	waitForTaskField(t, env, client, token, pid, base, 10*time.Second, func(row map[string]any) bool { return row["title"] == "resumed" })
	waitForAutomationRunStatus(t, env, client, token, pid, aid, "completed", 10*time.Second)
}

type resumeGraphValidator func(context.Context, uuid.UUID, uuid.UUID) (*automationdom.Graph, error)

func (v resumeGraphValidator) ValidateExecution(ctx context.Context, pid, nodeID uuid.UUID) (*automationdom.Graph, error) {
	return v(ctx, pid, nodeID)
}

func TestAutomationRunRetainsValidatedRevisionAfterConcurrentEdit(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "run-snapshot-manager", "password1")
	client, token := taskMemberLogin(t, env, "run-snapshot-manager", "password1")
	pid := createProjectForTasksViaAPI(t, env, client, token)
	task := createTaskViaAPI(t, env, client, token, pid, "run snapshot")
	aid := createAutomationViaAPI(t, env, client, token, pid, "run snapshot")
	trigger := addAutomationNodeViaAPI(t, env, client, token, pid, aid, "trigger", "api_trigger", map[string]any{"target_task_id": task})
	wait := addAutomationNodeViaAPI(t, env, client, token, pid, aid, "action", "wait", map[string]any{"wait_minutes": 1})
	addAutomationEdgeViaAPI(t, env, client, token, pid, aid, idOf(trigger), idOf(wait))
	activateAutomationViaAPI(t, env, client, token, pid, aid)
	secret := generateWebhookTokenViaAPI(t, env, client, token, pid, aid, idOf(trigger))
	original, err := env.automationRepo.LoadGraph(env.ctx, uuid.MustParse(aid))
	if err != nil {
		t.Fatal(err)
	}
	var validations atomic.Int32
	validator := resumeGraphValidator(func(ctx context.Context, pid, nodeID uuid.UUID) (*automationdom.Graph, error) {
		graph, err := env.automationSvc.ValidateExecution(ctx, pid, nodeID)
		if err != nil {
			return nil, err
		}
		// This occurs after the actual production validation returned its snapshot,
		// before the actual worker creates a run. The first call is the external
		// routing preflight; mutate after the second, execution-start validation.
		if validations.Add(1) == 2 {
			n, e := env.automationRepo.FindNodeByID(ctx, uuid.MustParse(idOf(wait)))
			if e != nil {
				return nil, e
			}
			n.Config = json.RawMessage(`{"wait_minutes":2}`)
			err = env.automationRepo.UpdateNode(ctx, n)
		}
		return graph, err
	})
	consumer := worker.NewAutomationConsumer(env.redisClient, env.automationRepo, env.taskRepo, env.taskSvc, env.activitySvc, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithExecutionValidator(validator).WithConsumerGroup("e2e.snapshot." + uuid.NewString())
	consumer.Start(env.ctx)
	defer consumer.Stop()
	postWebhookExpect(t, env, idOf(trigger), secret, 202)
	deadline := time.Now().Add(10 * time.Second)
	var runID uuid.UUID
	for time.Now().Before(deadline) {
		err = env.db.Get(&runID, `SELECT run_id FROM automation_pending_delays WHERE automation_id=$1`, aid)
		if err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	run, err := env.automationRepo.FindRunByID(env.ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := env.automationRepo.LoadGraph(env.ctx, uuid.MustParse(aid))
	if err != nil {
		t.Fatal(err)
	}
	if run.GraphRevision != original.Automation.GraphRevision || current.Automation.GraphRevision != run.GraphRevision+1 {
		t.Fatalf("run recorded wrong snapshot: original=%d run=%d current=%d", original.Automation.GraphRevision, run.GraphRevision, current.Automation.GraphRevision)
	}
}
