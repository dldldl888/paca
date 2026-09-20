package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	automationdom "github.com/Paca-AI/api/internal/domain/automation"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/events"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/database"
	"github.com/Paca-AI/api/migrations"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func TestAutomationTaskControlAccess(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "automation-manager", "password1")
	client, token := taskMemberLogin(t, env, "automation-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	user, _ := env.userRepo.FindByUsername(env.ctx, "automation-manager")
	manager := authz.WithActorID(env.ctx, user.ID)
	seedUser(t, env, "automation-editor", "password1", "editor")
	editor, _ := env.userRepo.FindByUsername(env.ctx, "automation-editor")
	member := uuid.New()
	if _, err := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, member, pid, editor.ID); err != nil {
		t.Fatal(err)
	}
	editorCtx := authz.WithActorID(env.ctx, editor.ID)
	task, err := env.taskSvc.CreateTask(manager, taskdom.CreateTaskInput{ProjectID: pid, Title: "private automation target"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := env.automationSvc.CreateAutomation(manager, automationdom.CreateAutomationInput{ProjectID: pid, Name: "private graph"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"target_task_id": task.ID})
	_, err = env.automationSvc.AddNode(manager, pid, a.ID, automationdom.AddNodeInput{Kind: automationdom.KindTrigger, Type: "api_trigger", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("editor_rich_read", func(t *testing.T) {
		if graph, err := env.automationSvc.GetAutomation(editorCtx, pid, a.ID); err == nil {
			t.Fatalf("editor read private graph: %+v", graph)
		}
	})
	t.Run("editor_mutation", func(t *testing.T) {
		if _, err := env.automationSvc.AddNode(editorCtx, pid, a.ID, automationdom.AddNodeInput{Kind: automationdom.KindAction, Type: "update_task", Config: json.RawMessage(`{}`)}); err == nil {
			t.Fatal("editor added executable node")
		}
	})
	graph, err := env.automationSvc.GetAutomation(manager, pid, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	trigger := graph.Nodes[0]
	action, err := env.automationSvc.AddNode(manager, pid, a.ID, automationdom.AddNodeInput{Kind: automationdom.KindAction, Type: "update_task", Config: json.RawMessage(`{"update":{"title":"managed automation update"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	edge, err := env.automationSvc.AddEdge(manager, pid, a.ID, automationdom.AddEdgeInput{SourceNodeID: trigger.ID, TargetNodeID: action.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.automationSvc.Activate(manager, pid, a.ID); err != nil {
		t.Fatal(err)
	}
	run := &automationdom.Run{ID: uuid.New(), AutomationID: a.ID, TriggerNodeID: trigger.ID, TaskID: &task.ID, Status: automationdom.RunStatusCompleted, StartedAt: time.Now()}
	if err := env.automationRepo.CreateRun(env.ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := env.automationRepo.CreateRunStep(env.ctx, &automationdom.RunStep{ID: uuid.New(), RunID: run.ID, NodeID: action.ID, Status: automationdom.RunStepCompleted, InputSnapshot: json.RawMessage(`{"secret":"private payload"}`), ExecutedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var out string
		if err := env.db.Get(&out, `SELECT jsonb_build_object('automations',(SELECT jsonb_agg(to_jsonb(x)) FROM automations x),'nodes',(SELECT jsonb_agg(to_jsonb(x)) FROM automation_nodes x),'edges',(SELECT jsonb_agg(to_jsonb(x)) FROM automation_edges x),'runs',(SELECT jsonb_agg(to_jsonb(x)) FROM automation_runs x),'tokens',(SELECT jsonb_agg(to_jsonb(x)) FROM automation_webhook_tokens x))::text`); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()
	eventsBefore := env.redisClient.XLen(env.ctx, events.StreamAutomationExternalTriggers).Val()
	name := "forged"
	newCfg := json.RawMessage(`{"update":{"title":"forged"}}`)
	for name, call := range map[string]func() error{
		"read_graph":   func() error { _, err := env.automationSvc.GetAutomation(editorCtx, pid, a.ID); return err },
		"runs":         func() error { _, err := env.automationSvc.ListRuns(editorCtx, pid, a.ID, 1); return err },
		"steps":        func() error { _, err := env.automationSvc.ListRunSteps(editorCtx, pid, a.ID, run.ID); return err },
		"dependencies": func() error { _, err := env.automationSvc.DependencyMap(editorCtx, pid); return err },
		"update_graph": func() error {
			_, err := env.automationSvc.UpdateAutomation(editorCtx, pid, a.ID, automationdom.UpdateAutomationInput{Name: &name})
			return err
		},
		"delete_graph": func() error { return env.automationSvc.DeleteAutomation(editorCtx, pid, a.ID) },
		"activate":     func() error { _, err := env.automationSvc.Activate(editorCtx, pid, a.ID); return err },
		"deactivate":   func() error { _, err := env.automationSvc.Deactivate(editorCtx, pid, a.ID); return err },
		"update_node": func() error {
			_, err := env.automationSvc.UpdateNode(editorCtx, pid, a.ID, action.ID, automationdom.UpdateNodeInput{Config: &newCfg})
			return err
		},
		"remove_node": func() error { return env.automationSvc.RemoveNode(editorCtx, pid, a.ID, action.ID) },
		"add_edge": func() error {
			_, err := env.automationSvc.AddEdge(editorCtx, pid, a.ID, automationdom.AddEdgeInput{SourceNodeID: trigger.ID, TargetNodeID: action.ID})
			return err
		},
		"remove_edge": func() error { return env.automationSvc.RemoveEdge(editorCtx, pid, a.ID, edge.ID) },
		"token": func() error {
			_, _, err := env.automationSvc.GenerateWebhookToken(editorCtx, pid, a.ID, trigger.ID, automationdom.WebhookTokenActor{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatal("editor operation allowed")
			}
			if after := snapshot(); after != before {
				t.Fatal("denial changed graph/token/run rows")
			}
		})
	}
	if env.redisClient.XLen(env.ctx, events.StreamAutomationExternalTriggers).Val() != eventsBefore {
		t.Fatal("denial published execution")
	}
	// A stored active graph must be reauthorized after the migration, preserving history.
	if _, err := env.db.Exec(`DELETE FROM schema_migrations WHERE filename='000055_reauthorize_automations.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := database.RunMigrationsFS(env.db.DB, migrations.FS); err != nil {
		t.Fatal(err)
	}
	current, err := env.automationRepo.FindAutomationByID(env.ctx, a.ID)
	if err != nil || current.Status != automationdom.StatusInactive {
		t.Fatalf("cutover status %+v %v", current, err)
	}
	if runs, err := env.automationSvc.ListRuns(manager, pid, a.ID, 10); err != nil || len(runs) != 1 {
		t.Fatalf("history lost: %d %v", len(runs), err)
	}
	if _, err := env.automationSvc.Activate(manager, pid, a.ID); err != nil {
		t.Fatal(err)
	}
	// Stored foreign references and plugin nodes fail before activation/dispatch.
	plugin := &automationdom.Node{ID: uuid.New(), AutomationID: a.ID, Kind: automationdom.KindAction, Type: "plugin.raw_tasks", Config: json.RawMessage(`{}`), CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := env.automationRepo.CreateNode(env.ctx, plugin); err != nil {
		t.Fatal(err)
	}
	if _, err := env.automationSvc.Activate(manager, pid, a.ID); err == nil {
		t.Fatal("plugin graph activated")
	}
	if _, err := env.automationSvc.ValidateExecution(env.ctx, pid, trigger.ID); err == nil {
		t.Fatal("plugin graph executable")
	}
	if err := env.automationRepo.DeleteNode(env.ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	var managerMember, oldRole uuid.UUID
	if err := env.db.QueryRow(`SELECT id,project_role_id FROM project_members WHERE project_id=$1 AND user_id=$2`, pid, user.ID).Scan(&managerMember, &oldRole); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE project_members SET project_role_id=(SELECT id FROM project_roles WHERE project_id=$1 AND role_name='Editor') WHERE id=$2`, pid, managerMember); err != nil {
		t.Fatal(err)
	}
	if _, err := env.automationSvc.GetAutomation(manager, pid, a.ID); err == nil {
		t.Fatal("old manager role retained graph authority")
	}
	if _, err := env.db.Exec(`UPDATE project_members SET project_role_id=$1 WHERE id=$2`, oldRole, managerMember); err != nil {
		t.Fatal(err)
	}
	if _, err := env.automationSvc.GetAutomation(manager, pid, a.ID); err != nil {
		t.Fatal(err)
	}

	tok, secret, err := env.automationSvc.GenerateWebhookToken(manager, pid, a.ID, trigger.ID, automationdom.WebhookTokenActor{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE projects SET deleted_at=NOW() WHERE id=$1`, pid); err != nil {
		t.Fatal(err)
	}
	if err := env.automationSvc.HandleWebhookTrigger(env.ctx, trigger.ID, secret); err == nil {
		t.Fatal("deleted project webhook accepted")
	}
	var used bool
	if err := env.db.Get(&used, `SELECT last_used_at IS NOT NULL FROM automation_webhook_tokens WHERE id=$1`, tok.ID); err != nil {
		t.Fatal(err)
	}
	if used || env.redisClient.XLen(env.ctx, events.StreamAutomationExternalTriggers).Val() != eventsBefore {
		t.Fatal("deleted project webhook had effects")
	}

}

func TestAgentActivityTaskSQLAccess(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "feed-manager", "password1")
	client, token := taskMemberLogin(t, env, "feed-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	user, _ := env.userRepo.FindByUsername(env.ctx, "feed-manager")
	manager := authz.WithActorID(env.ctx, user.ID)
	var roleID uuid.UUID
	if err := env.db.Get(&roleID, `SELECT id FROM project_roles WHERE project_id=$1 AND role_name='Editor'`, pid); err != nil {
		t.Fatal(err)
	}
	var members [2]uuid.UUID
	var contexts [2]context.Context
	for i, name := range []string{"feed-a", "feed-b"} {
		seedUser(t, env, name, "password1", name)
		u, _ := env.userRepo.FindByUsername(env.ctx, name)
		members[i] = uuid.New()
		contexts[i] = authz.WithActorID(env.ctx, u.ID)
		if _, err := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) VALUES($1,$2,$3,$4,'human')`, members[i], pid, u.ID, roleID); err != nil {
			t.Fatal(err)
		}
	}
	ag, err := env.agentSvc.CreateAgent(manager, pid, agentdom.CreateAgentInput{Name: "feed agent", Handle: "feed-agent", LLMProvider: "test", LLMModel: "test", ProjectRoleID: roleID})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := env.projectRepo.FindMemberByAgent(env.ctx, pid, ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	baseTime := time.Now().Add(-time.Hour)
	var allowed, hidden uuid.UUID
	for i := 0; i < 107; i++ {
		id := uuid.New()
		owner := members[0]
		title := fmt.Sprintf("allowed-%03d", i)
		if i == 105 {
			owner = members[1]
			title = "HIDDEN_TITLE"
			hidden = id
		}
		if i == 0 {
			allowed = id
		}
		exec(`INSERT INTO tasks(id,project_id,task_number,title) VALUES($1,$2,$3,$4)`, id, pid, i+1, title)
		exec(`INSERT INTO task_assignees(task_id,member_id) VALUES($1,$2)`, id, owner)
		exec(`INSERT INTO task_activities(id,task_id,actor_id,activity_type,content,created_at) VALUES($1,$2,$3,'task.comment.added',$4,$5)`, uuid.New(), id, actor.ID, `{"body":"`+title+`"}`, baseTime.Add(time.Duration(i)*time.Second))
		if i == 106 {
			exec(`UPDATE tasks SET deleted_at=NOW() WHERE id=$1`, id)
		}
	}
	// The visible source must not carry an unreadable typed reference into search/cursors.
	for _, row := range []struct{ kind, content string }{
		{"task.link.added", `{"target_task_id":"` + hidden.String() + `"}`},
		{"task.updated", `{"changes":[{"field":"parent_task_id","old":"` + hidden.String() + `","new":null}]}`},
		{"task.updated", `{"changes":[{"field":"parent_task","old":null,"new":"` + hidden.String() + `"}]}`},
		{"automation.applied", `{"parent_task_id":"` + hidden.String() + `"}`},
	} {
		exec(`INSERT INTO task_activities(id,task_id,actor_id,activity_type,content,created_at) VALUES($1,$2,$3,$4,$5,$6)`, uuid.New(), allowed, actor.ID, row.kind, row.content, baseTime.Add(200*time.Second))
	}
	docID := uuid.New()
	exec(`INSERT INTO documents(visibility,id,project_id,title) VALUES('legacy_project',$1,$2,'permitted doc')`, docID, pid)
	exec(`INSERT INTO doc_activities(id,document_id,actor_id,activity_type,content,created_at) VALUES($1,$2,$3,'doc.updated','{"body":"doc body"}',$4)`, uuid.New(), docID, actor.ID, baseTime.Add(300*time.Second))
	filter := agentdom.ListAgentActivitiesFilter{ProjectID: pid, ActorMemberID: actor.ID}
	list := func(ctx context.Context, f agentdom.ListAgentActivitiesFilter, limit int) ([]*agentdom.ActivityFeedItem, bool) {
		t.Helper()
		items, more, err := env.agentSvc.ListAgentActivities(ctx, f, limit)
		if err != nil {
			t.Fatal(err)
		}
		return items, more
	}
	items, more := list(contexts[0], filter, 106)
	if len(items) != 106 || more || items[0].SourceID != docID {
		t.Fatalf("full authorized page len=%d more=%v", len(items), more)
	}
	raw, _ := json.Marshal(items)
	if strings.Contains(string(raw), hidden.String()) || strings.Contains(string(raw), "HIDDEN_TITLE") {
		t.Fatal("hidden task/ref in full page")
	}
	page, more := list(contexts[0], filter, 50)
	if len(page) != 50 || !more {
		t.Fatalf("first page %d more=%v", len(page), more)
	}
	cur := agentdom.EncodeActivityFeedCursor(page[49])
	filter.CursorAfter = &cur
	page, more = list(contexts[0], filter, 56)
	if len(page) != 56 || more {
		t.Fatalf("second page %d more=%v", len(page), more)
	}
	filter.CursorAfter = nil
	for _, q := range []string{"HIDDEN_TITLE", hidden.String()} {
		filter.Search = &q
		page, more = list(contexts[0], filter, 1)
		if len(page) != 0 || more {
			t.Fatal("hidden search affects results/hasMore")
		}
	}
	filter.Search = nil
	after := baseTime.Add(150 * time.Second)
	filter.CreatedAfter = &after
	page, more = list(contexts[0], filter, 1)
	if len(page) != 1 || page[0].SourceID != docID || more {
		t.Fatal("hidden typed refs affect date pagination")
	}
	filter.CreatedAfter = nil
	page, more = list(contexts[1], filter, 5)
	if len(page) != 2 || more {
		t.Fatalf("B got A tasks: %d more=%v", len(page), more)
	}
	// Current assignment changes take effect without replacing the auth context.
	exec(`DELETE FROM task_assignees WHERE task_id=$1`, allowed)
	q := "allowed-000"
	filter.Search = &q
	page, more = list(contexts[0], filter, 1)
	if len(page) != 0 || more {
		t.Fatal("removed assignment retained feed source")
	}
	exec(`INSERT INTO task_assignees(task_id,member_id) VALUES($1,$2)`, allowed, members[1])
	page, _ = list(contexts[1], filter, 1)
	if len(page) != 1 || page[0].SourceID != allowed {
		t.Fatal("new assignee lacks source")
	}
	// Caller-supplied visibility cannot replace current human authority.
	filter.Visibility = taskdom.AccessScope{ProjectID: pid, ReadAll: true}
	filter.Search = nil
	page, _ = list(contexts[0], filter, 200)
	if len(page) != 105 {
		t.Fatalf("forged scope broadened feed: %d", len(page))
	}
	exec(`UPDATE project_members SET deleted_at=NOW() WHERE id=$1`, members[0])
	if _, _, err := env.agentSvc.ListAgentActivities(contexts[0], filter, 1); err == nil {
		t.Fatal("removed member retained feed access")
	}
	if items, more, err := env.agentRepo.ListAgentActivities(env.ctx, agentdom.ListAgentActivitiesFilter{ActorMemberID: actor.ID}, 1); err != nil || len(items) != 0 || more {
		t.Fatalf("unscoped feed items=%d more=%v err=%v", len(items), more, err)
	}
}

func TestAgentDescriptionWriteTaskManagement(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "description-manager", "password1")
	client, token := taskMemberLogin(t, env, "description-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	managerUser, _ := env.userRepo.FindByUsername(env.ctx, "description-manager")
	manager := authz.WithActorID(env.ctx, managerUser.ID)
	var roleID, managerMember uuid.UUID
	if err := env.db.Get(&roleID, `SELECT id FROM project_roles WHERE project_id=$1 AND role_name='Editor'`, pid); err != nil {
		t.Fatal(err)
	}
	if err := env.db.Get(&managerMember, `SELECT id FROM project_members WHERE project_id=$1 AND user_id=$2`, pid, managerUser.ID); err != nil {
		t.Fatal(err)
	}
	seedUser(t, env, "description-reader", "password1", "reader")
	readerUser, _ := env.userRepo.FindByUsername(env.ctx, "description-reader")
	readerMember := uuid.New()
	reader := authz.WithActorID(env.ctx, readerUser.ID)
	if _, err := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) VALUES($1,$2,$3,$4,'human')`, readerMember, pid, readerUser.ID, roleID); err != nil {
		t.Fatal(err)
	}
	ag, err := env.agentSvc.CreateAgent(manager, pid, agentdom.CreateAgentInput{Name: "description agent", Handle: "description-agent", LLMProvider: "test", LLMModel: "test", ProjectRoleID: roleID})
	if err != nil {
		t.Fatal(err)
	}
	task, err := env.taskSvc.CreateTask(manager, taskdom.CreateTaskInput{ProjectID: pid, Title: "managed description", AssigneeIDs: []uuid.UUID{readerMember}})
	if err != nil {
		t.Fatal(err)
	}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := env.db.Get(&n, `SELECT count(*) FROM `+table); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before, queueBefore := count("agent_conversations"), count("agent_pending_triggers")
	eventsBefore := env.redisClient.XLen(env.ctx, events.StreamAgentTriggers).Val()
	for _, ctx := range []context.Context{reader, env.ctx} {
		if conv, err := env.agentSvc.TriggerDescriptionWrite(ctx, pid, ag.ID, task.ID, managerMember); err == nil || conv != nil {
			t.Fatalf("unmanaged description allowed: %+v %v", conv, err)
		}
	}
	if count("agent_conversations") != before || count("agent_pending_triggers") != queueBefore || env.redisClient.XLen(env.ctx, events.StreamAgentTriggers).Val() != eventsBefore {
		t.Fatal("denied description had effects")
	}
	conv, err := env.agentSvc.TriggerDescriptionWrite(manager, pid, ag.ID, task.ID, readerMember)
	if err != nil {
		t.Fatal(err)
	}
	if conv.TriggeredByMemberID == nil || *conv.TriggeredByMemberID != managerMember || conv.TaskID == nil || *conv.TaskID != task.ID {
		t.Fatalf("caller forged description attribution: %+v", conv)
	}
	if count("agent_conversations") != before+1 || env.redisClient.XLen(env.ctx, events.StreamAgentTriggers).Val() != eventsBefore+1 {
		t.Fatal("authorized description did not dispatch exactly once")
	}
	if _, err := env.db.Exec(`UPDATE project_members SET deleted_at=NOW() WHERE id=$1`, managerMember); err != nil {
		t.Fatal(err)
	}
	if conv, err := env.agentSvc.TriggerDescriptionWrite(manager, pid, ag.ID, task.ID, managerMember); err == nil || conv != nil {
		t.Fatal("removed manager retained description authority")
	}
	if count("agent_conversations") != before+1 {
		t.Fatal("revoked description created conversation")
	}
}

// The public entry must inspect the complete relation before marking a token used.
// The foreign child sorts after the old UI list's 200-row cap.
func TestAutomationWebhookLateForeignChild(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "webhook-manager", "password1")
	client, token := taskMemberLogin(t, env, "webhook-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	foreignProject := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	user, err := env.userRepo.FindByUsername(env.ctx, "webhook-manager")
	if err != nil {
		t.Fatal(err)
	}
	manager := authz.WithActorID(env.ctx, user.ID)
	base, err := env.taskSvc.CreateTask(manager, taskdom.CreateTaskInput{ProjectID: pid, Title: "webhook parent"})
	if err != nil {
		t.Fatal(err)
	}
	// Raw seeding deliberately creates the foreign relation that ordinary writes reject.
	for i := 1; i <= 201; i++ {
		id := uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
		if _, err := env.db.Exec(`INSERT INTO tasks(id,project_id,title,parent_task_id,task_number,status_id) SELECT $1,$2,'unchanged',$3,$4,status_id FROM tasks WHERE id=$3`, id, pid, base.ID, i+1); err != nil {
			t.Fatal(err)
		}
	}
	foreignID := uuid.MustParse("ffffffff-ffff-4fff-bfff-ffffffffffff")
	if _, err := env.db.Exec(`INSERT INTO tasks(id,project_id,title,parent_task_id,task_number) VALUES($1,$2,'unchanged',$3,1)`, foreignID, foreignProject, base.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := env.taskRepo.ListChildTaskIDs(env.ctx, base.ID)
	if err != nil || len(ids) != 202 || ids[201] != foreignID {
		t.Fatalf("complete relation: len=%d err=%v", len(ids), err)
	}
	a, err := env.automationSvc.CreateAutomation(manager, automationdom.CreateAutomationInput{ProjectID: pid, Name: "complete webhook fanout"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"target_task_id": base.ID})
	trigger, err := env.automationSvc.AddNode(manager, pid, a.ID, automationdom.AddNodeInput{Kind: automationdom.KindTrigger, Type: "api_trigger", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	action, err := env.automationSvc.AddNode(manager, pid, a.ID, automationdom.AddNodeInput{Kind: automationdom.KindAction, Type: "update_task", Config: json.RawMessage(`{"target":{"kind":"children"},"update":{"title":"authorized fanout"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.automationSvc.AddEdge(manager, pid, a.ID, automationdom.AddEdgeInput{SourceNodeID: trigger.ID, TargetNodeID: action.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.automationSvc.Activate(manager, pid, a.ID); err != nil {
		t.Fatal(err)
	}
	tok, secret, err := env.automationSvc.GenerateWebhookToken(manager, pid, a.ID, trigger.ID, automationdom.WebhookTokenActor{})
	if err != nil {
		t.Fatal(err)
	}
	before := env.redisClient.XLen(env.ctx, events.StreamAutomationExternalTriggers).Val()
	if err := env.automationSvc.HandleWebhookTrigger(env.ctx, trigger.ID, secret); err == nil {
		t.Fatal("late foreign child accepted")
	}
	var used bool
	if err := env.db.Get(&used, `SELECT last_used_at IS NOT NULL FROM automation_webhook_tokens WHERE id=$1`, tok.ID); err != nil {
		t.Fatal(err)
	}
	var runs, changed int
	if err := env.db.Get(&runs, `SELECT count(*) FROM automation_runs WHERE automation_id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.db.Get(&changed, `SELECT count(*) FROM tasks WHERE parent_task_id=$1 AND title<>'unchanged'`, base.ID); err != nil {
		t.Fatal(err)
	}
	if used || runs != 0 || changed != 0 || env.redisClient.XLen(env.ctx, events.StreamAutomationExternalTriggers).Val() != before {
		t.Fatalf("denial effects: token=%v runs=%d changed=%d", used, runs, changed)
	}
	if _, err := env.db.Exec(`DELETE FROM tasks WHERE id=$1`, foreignID); err != nil {
		t.Fatal(err)
	}
	startAutomationConsumer(t, env)
	if err := env.automationSvc.HandleWebhookTrigger(env.ctx, trigger.ID, secret); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if err := env.db.Get(&changed, `SELECT count(*) FROM tasks WHERE parent_task_id=$1 AND title='authorized fanout'`, base.ID); err != nil {
			t.Fatal(err)
		}
		if err := env.db.Get(&runs, `SELECT count(*) FROM automation_runs WHERE automation_id=$1 AND status='completed'`, a.ID); err != nil {
			t.Fatal(err)
		}
		if changed == 201 && runs == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if changed != 201 || runs != 1 {
		t.Fatalf("actual TaskService fanout incomplete: changed=%d runs=%d", changed, runs)
	}
	if err := env.db.Get(&used, `SELECT last_used_at IS NOT NULL FROM automation_webhook_tokens WHERE id=$1`, tok.ID); err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Fatal("accepted token was not recorded")
	}
}
