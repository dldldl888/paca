package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	automationdom "github.com/Paca-AI/api/internal/domain/automation"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/events"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/database"
	"github.com/Paca-AI/api/internal/platform/messaging"
	pgRepo "github.com/Paca-AI/api/internal/repository/postgres"
	agentsvc "github.com/Paca-AI/api/internal/service/agent"
	annotationsvc "github.com/Paca-AI/api/internal/service/annotation"
	docsvc "github.com/Paca-AI/api/internal/service/doc"
	"github.com/Paca-AI/api/internal/transport/http/handler"
	"github.com/Paca-AI/api/migrations"
	"github.com/google/uuid"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrivateRunRejectsInaccessibleContextBeforeEffects(t *testing.T) {
	env := newE2EEnv(t)
	env.agentSvc.WithAuthorizer(authz.NewAuthorizer(pgRepo.NewAuthzPermissionStore(env.db))).WithPrivateRuns(uuid.New(), env.agentRepo, nil, nil, env.automationSvc)
	client, token, project, role := seedACPUser(t, env)
	status, result := createAgentRequest(t, env, client, token, project, acpAgentBody(role, "private-check", nil))
	if status != http.StatusCreated {
		t.Fatalf("create agent: %d", status)
	}
	aid := uuid.MustParse(assertDataMap(t, result)["id"].(string))
	pid := uuid.MustParse(project)
	var member, user uuid.UUID
	if err := env.db.QueryRow(`SELECT id,user_id FROM project_members WHERE project_id=$1 AND member_type='human' LIMIT 1`, pid).Scan(&member, &user); err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithActorID(env.ctx, user)
	session, conv, err := env.agentSvc.StartChatSession(ctx, pid, aid, member, "denied", nil, nil, []agentdom.ContextItemRef{{Type: agentdom.ContextItemTask, ID: uuid.NewString()}}, "")
	if err == nil || session != nil || conv != nil {
		t.Fatalf("inaccessible context accepted: session=%v conversation=%v err=%v", session, conv, err)
	}
	var n int
	if err := env.db.Get(&n, `SELECT count(*) FROM agent_chat_sessions WHERE agent_id=$1`, aid); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("denied request wrote %d sessions", n)
	}
}

func TestPrivatePersistencePreservesPriorTurnLatch(t *testing.T) {
	env := newE2EEnv(t)
	env.agentSvc.WithAuthorizer(authz.NewAuthorizer(pgRepo.NewAuthzPermissionStore(env.db))).WithPrivateRuns(uuid.New(), env.agentRepo, nil, nil, env.automationSvc)
	client, token, project, role := seedACPUser(t, env)
	_, result := createAgentRequest(t, env, client, token, project, acpAgentBody(role, "latch", nil))
	aid := uuid.MustParse(assertDataMap(t, result)["id"].(string))
	pid := uuid.MustParse(project)
	var member, user uuid.UUID
	if err := env.db.QueryRow(`SELECT id,user_id FROM project_members WHERE project_id=$1 AND member_type='human' LIMIT 1`, pid).Scan(&member, &user); err != nil {
		t.Fatal(err)
	}
	_, c, err := env.agentSvc.StartChatSession(authz.WithActorID(env.ctx, user), pid, aid, member, "first", nil, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE agent_conversations SET started_at=now() WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.agentRepo.UpdateConversation(env.ctx, c); err != nil {
		t.Fatal(err)
	}
	fresh, err := env.agentRepo.FindConversationByID(env.ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.StartedAt == nil {
		t.Fatal("stale full-row update cleared prior-turn latch")
	}
	// Reapply the actual migration to a history row that predates metadata.
	if _, err := env.db.Exec(`ALTER TABLE agent_conversations DROP COLUMN context_items; DELETE FROM schema_migrations WHERE filename='000057_add_conversation_context_items.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := database.RunMigrationsFS(env.db.DB, migrations.FS); err != nil {
		t.Fatal(err)
	}
	legacy, err := env.agentRepo.FindConversationByID(env.ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.ContextItems != nil {
		t.Fatal("migration certified old history")
	}
	if _, err := env.agentSvc.AuthorizeRun(env.ctx, c.ID, aid, false); err == nil {
		t.Fatal("legacy history authorized")
	}

}

func TestPrivateRunCurrentAuthorization(t *testing.T) {
	env := newE2EEnv(t)
	client, token, project, role := seedACPUser(t, env)
	pid := uuid.MustParse(project)
	_, result := createAgentRequest(t, env, client, token, project, acpAgentBody(role, "authority", nil))
	aid := uuid.MustParse(assertDataMap(t, result)["id"].(string))
	var manager, managerMember uuid.UUID
	if err := env.db.QueryRow(`SELECT user_id,id FROM project_members WHERE project_id=$1 AND member_type='human'`, pid).Scan(&manager, &managerMember); err != nil {
		t.Fatal(err)
	}
	managerCtx := authz.WithActorID(env.ctx, manager)
	seedUser(t, env, "private-owner", "password1", "Private Owner")
	owner, _ := env.userRepo.FindByUsername(env.ctx, "private-owner")
	member := uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) VALUES($1,$2,$3,$4,'human')`, member, pid, owner.ID, role)
	ownerCtx := authz.WithActorID(env.ctx, owner.ID)
	ar := pgRepo.NewAnnotationRepository(env.db)
	annotations := annotationsvc.New(ar, nil, env.taskSvc, env.attachmentRepo, env.attachmentRepo, &boundaryStorage{}, "test").WithTaskAccess(env.taskSvc, env.taskSvc)
	docs := docsvc.New(pgRepo.NewDocumentRepository(env.db), env.projectRepo)
	instance := uuid.New()
	env.agentSvc.WithAuthorizer(authz.NewAuthorizer(pgRepo.NewAuthzPermissionStore(env.db))).WithPrivateRuns(instance, env.agentRepo, docs, annotations, env.automationSvc)
	task, err := env.taskSvc.CreateTask(managerCtx, taskdom.CreateTaskInput{ProjectID: pid, Title: "owner context", AssigneeIDs: []uuid.UUID{member}})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := env.taskSvc.CreateTask(managerCtx, taskdom.CreateTaskInput{ProjectID: pid, Title: "hidden context"})
	if err != nil {
		t.Fatal(err)
	}
	ref := func(kind agentdom.ContextItemType, id uuid.UUID) agentdom.ContextItemRef {
		return agentdom.ContextItemRef{Type: kind, ID: id.String()}
	}
	start := func(ctx context.Context, mid uuid.UUID, refs ...agentdom.ContextItemRef) *agentdom.AgentConversation {
		t.Helper()
		_, c, e := env.agentSvc.StartChatSession(ctx, pid, aid, mid, "hello", nil, nil, refs, "queue")
		if e != nil {
			t.Fatal(e)
		}
		exec(`UPDATE agent_conversations SET status='finished' WHERE id=$1`, c.ID)
		c.Status = "finished"
		return c
	}
	allowed := func(c *agentdom.AgentConversation) {
		t.Helper()
		a, e := env.agentSvc.AuthorizeRun(env.ctx, c.ID, aid, false)
		if e != nil || a.OwnerUserID == "" || a.InstanceID != instance.String() {
			t.Fatalf("expected authorized private run: %+v %v", a, e)
		}
	}
	denied := func(c *agentdom.AgentConversation) {
		t.Helper()
		if a, e := env.agentSvc.AuthorizeRun(env.ctx, c.ID, aid, false); e == nil || a != nil {
			t.Fatalf("unexpected authorization: %+v %v", a, e)
		}
	}
	c := start(ownerCtx, member, ref(agentdom.ContextItemTask, task.ID))
	allowed(c)
	t.Run("owner_history_is_separate_from_generation", func(t *testing.T) {
		exec(`DELETE FROM task_assignees WHERE task_id=$1`, task.ID)
		denied(c)
		if _, e := env.agentSvc.GetConversation(ownerCtx, pid, c.ID, member); e != nil {
			t.Fatalf("owner history lost: %v", e)
		}
		for _, send := range []func() error{
			func() error {
				return env.agentSvc.SendConversationMessage(ownerCtx, pid, c.ID, "resume", member, nil, "")
			},
			func() error {
				_, e := env.agentSvc.SendChatMessage(ownerCtx, pid, *c.ChatSessionID, member, "resume", nil, "")
				return e
			},
		} {
			if send() == nil {
				t.Fatal("revoked context resumed")
			}
		}
		fresh, _ := env.agentRepo.FindConversationByID(env.ctx, c.ID)
		if fresh.Status != "finished" {
			t.Fatal("denied resume changed status")
		}
		exec(`INSERT INTO task_assignees(task_id,member_id) VALUES($1,$2)`, task.ID, member)
		allowed(c)
	})
	t.Run("manager_cannot_read_control_or_search_other_private_chat", func(t *testing.T) {
		if _, e := env.agentSvc.GetConversation(managerCtx, pid, c.ID, managerMember); e == nil {
			t.Fatal("manager read private history")
		}
		if e := env.agentSvc.StopConversation(managerCtx, pid, c.ID, managerMember); e == nil {
			t.Fatal("manager controlled private chat")
		}
		exec(`UPDATE agent_chat_sessions SET title='private-marker' WHERE id=$1`, *c.ChatSessionID)
		search := "private-marker"
		rows, _, e := env.agentRepo.ListConversations(managerCtx, agentdom.ListConversationsFilter{ProjectID: &pid, ViewerMemberID: &managerMember, Search: &search}, 20)
		if e != nil {
			t.Fatal(e)
		}
		for _, row := range rows {
			if row.ID == c.ID {
				t.Fatal("manager search leaked private chat")
			}
		}
	})
	t.Run("current_membership_user_project_role_and_agent", func(t *testing.T) {
		for _, change := range [][2]string{
			{`UPDATE project_members SET deleted_at=now() WHERE id=$1`, `UPDATE project_members SET deleted_at=NULL WHERE id=$1`},
			{`UPDATE users SET deleted_at=now() WHERE id=$1`, `UPDATE users SET deleted_at=NULL WHERE id=$1`},
			{`UPDATE projects SET deleted_at=now() WHERE id=$1`, `UPDATE projects SET deleted_at=NULL WHERE id=$1`},
			{`UPDATE agents SET deleted_at=now() WHERE id=$1`, `UPDATE agents SET deleted_at=NULL WHERE id=$1`},
		} {
			id := member
			if strings.Contains(change[0], "users") {
				id = owner.ID
			}
			if strings.Contains(change[0], "projects") {
				id = pid
			}
			if strings.Contains(change[0], "agents") {
				id = aid
			}
			exec(change[0], id)
			denied(c)
			exec(change[1], id)
			allowed(c)
		}
		var permissions []byte
		if e := env.db.Get(&permissions, `SELECT permissions FROM project_roles WHERE id=$1`, role); e != nil {
			t.Fatal(e)
		}
		exec(`UPDATE project_roles SET permissions='["tasks.read"]' WHERE id=$1`, role)
		denied(c)
		exec(`UPDATE project_roles SET permissions=$1 WHERE id=$2`, permissions, role)
		allowed(c)
	})
	t.Run("missing_ownership_and_legacy_history_fail_closed", func(t *testing.T) {
		legacy := start(ownerCtx, member)
		exec(`UPDATE agent_conversations SET context_items=NULL WHERE id=$1`, legacy.ID)
		denied(legacy)
		if _, e := env.agentSvc.GetConversation(ownerCtx, pid, legacy.ID, member); e != nil {
			t.Fatal("legacy history unreadable")
		}
		if e := env.agentRepo.MergeConversationContext(env.ctx, legacy.ID, []agentdom.ContextItemRef{}); e == nil {
			t.Fatal("legacy metadata certified")
		}
		exec(`UPDATE agent_conversations SET chat_session_id=NULL WHERE id=$1`, legacy.ID)
		denied(legacy)
	})
	t.Run("all_shared_triggers_rejected_before_effects", func(t *testing.T) {
		var before, after int
		env.db.Get(&before, `SELECT count(*) FROM agent_conversations`)
		for _, call := range []func() error{
			func() error {
				_, e := env.agentSvc.TriggerTaskAssigned(managerCtx, pid, aid, task.ID, &managerMember, "")
				return e
			},
			func() error {
				_, e := env.agentSvc.TriggerDirectMessage(managerCtx, pid, aid, &managerMember, "autonomous")
				return e
			},
			func() error {
				_, e := env.agentSvc.TriggerCommentMention(managerCtx, pid, aid, task.ID, uuid.New(), managerMember, "mention")
				return e
			},
			func() error {
				_, e := env.agentSvc.TriggerDescriptionWrite(managerCtx, pid, aid, task.ID, managerMember)
				return e
			},
		} {
			if call() == nil {
				t.Fatal("shared trigger accepted")
			}
		}
		env.db.Get(&after, `SELECT count(*) FROM agent_conversations`)
		if after != before {
			t.Fatal("shared trigger had side effects")
		}
	})
	t.Run("browser_actor_and_scope_spoofing", func(t *testing.T) {
		if _, _, e := env.agentSvc.StartChatSession(managerCtx, pid, aid, member, "spoof", nil, nil, nil, ""); e == nil {
			t.Fatal("member attribution became owner")
		}
		wrong := uuid.NewString()
		r := ref(agentdom.ContextItemTask, task.ID)
		r.ProjectID = &wrong
		if _, _, e := env.agentSvc.StartChatSession(ownerCtx, pid, aid, member, "spoof", nil, nil, []agentdom.ContextItemRef{r}, ""); e == nil {
			t.Fatal("foreign project ref accepted")
		}
	})
	t.Run("conversation_refs_snapshot_and_dynamic_cycle_dependencies", func(t *testing.T) {
		source := start(ownerCtx, member, ref(agentdom.ContextItemTask, task.ID))
		target := start(ownerCtx, member, ref(agentdom.ContextItemConversation, source.ID))
		allowed(target)
		if e := env.agentRepo.MergeConversationContext(env.ctx, source.ID, []agentdom.ContextItemRef{ref(agentdom.ContextItemConversation, target.ID)}); e != nil {
			t.Fatal(e)
		}
		allowed(target)
		exec(`UPDATE agent_conversations SET task_id=$1 WHERE id=$2`, hidden.ID, source.ID)
		denied(target)
		exec(`UPDATE agent_conversations SET task_id=NULL,context_items='[]' WHERE id=$1`, source.ID)
		exec(`DELETE FROM task_assignees WHERE task_id=$1`, task.ID)
		denied(target)
		exec(`INSERT INTO task_assignees(task_id,member_id) VALUES($1,$2)`, task.ID, member)
		allowed(target)
	})
	t.Run("doc_annotation_current_task_and_rich_automation", func(t *testing.T) {
		doc := uuid.New()
		exec(`INSERT INTO documents(id,project_id,title) VALUES($1,$2,'real document title')`, doc, pid)
		d := start(ownerCtx, member, ref(agentdom.ContextItemDoc, doc))
		allowed(d)
		exec(`UPDATE documents SET deleted_at=now() WHERE id=$1`, doc)
		denied(d)
		eid, pfid, annotation := uuid.New(), uuid.New(), uuid.New()
		exec(`INSERT INTO environments(id,project_id,name,slug,backend,secret_key_encrypted) VALUES($1,$2,'test','test','docker','synthetic')`, eid, pid)
		exec(`INSERT INTO environment_port_forwards(id,environment_id,label,container_port) VALUES($1,$2,'test',3000)`, pfid, eid)
		exec(`INSERT INTO page_annotations(id,project_id,environment_id,port_forward_id,page_path,element_selector,bounding_box,element_snapshot,body,created_by) VALUES($1,$2,$3,$4,'/','#x','{}','{}','comment',$5)`, annotation, pid, eid, pfid, manager)
		a := start(ownerCtx, member, ref(agentdom.ContextItemAnnotation, annotation))
		allowed(a)
		if e := ar.SetTaskID(env.ctx, annotation, hidden.ID); e != nil {
			t.Fatal(e)
		}
		denied(a)
		auto, e := env.automationSvc.CreateAutomation(managerCtx, automationdom.CreateAutomationInput{ProjectID: pid, Name: "graph"})
		if e != nil {
			t.Fatal(e)
		}
		if _, _, e = env.agentSvc.StartChatSession(ownerCtx, pid, aid, member, "graph", nil, nil, []agentdom.ContextItemRef{ref(agentdom.ContextItemAutomation, auto.ID)}, ""); e == nil {
			t.Fatal("rich automation read_all bypassed")
		}
		allowed(start(managerCtx, managerMember, ref(agentdom.ContextItemAutomation, auto.ID)))
	})
	t.Run("queued_revocation_fails_and_releases_queue", func(t *testing.T) {
		busy := start(ownerCtx, member)
		exec(`UPDATE agent_conversations SET status='running' WHERE id=$1`, busy.ID)
		_, queued, e := env.agentSvc.StartChatSession(ownerCtx, pid, aid, member, "queued", nil, nil, []agentdom.ContextItemRef{ref(agentdom.ContextItemTask, task.ID)}, "queue")
		if e != nil {
			t.Fatal(e)
		}
		exec(`DELETE FROM task_assignees WHERE task_id=$1`, task.ID)
		exec(`UPDATE agent_conversations SET status='finished' WHERE id=$1`, busy.ID)
		before, _ := env.redisClient.XLen(env.ctx, events.StreamAgentTriggers).Result()
		if _, e = env.agentSvc.AdvanceQueue(env.ctx, aid, 10); e != nil {
			t.Fatal(e)
		}
		fresh, e := env.agentRepo.FindConversationByID(env.ctx, queued.ID)
		if e != nil || fresh.Status != "failed" {
			t.Fatalf("queue not terminal: %+v %v", fresh, e)
		}
		after, _ := env.redisClient.XLen(env.ctx, events.StreamAgentTriggers).Result()
		if before != after {
			t.Fatal("revoked queue published prompt")
		}
		var pending int
		env.db.Get(&pending, `SELECT count(*) FROM agent_pending_triggers WHERE conversation_id=$1`, queued.ID)
		if pending != 0 {
			t.Fatal("failed pending row retained")
		}
		exec(`INSERT INTO task_assignees(task_id,member_id) VALUES($1,$2)`, task.ID, member)
	})
	t.Run("first_dispatch_latch_and_atomic_cumulative_refs", func(t *testing.T) {
		fresh := start(ownerCtx, member)
		exec(`UPDATE agent_conversations SET status='running' WHERE id=$1`, fresh.ID)
		var first atomic.Int32
		errs := make(chan error, 32)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				a, e := env.agentSvc.AuthorizeRun(env.ctx, fresh.ID, aid, true)
				if e == nil && !a.HasPriorTurns {
					first.Add(1)
				}
				errs <- e
			}()
		}
		wg.Wait()
		if first.Load() != 1 {
			t.Fatalf("first dispatch observed %d times", first.Load())
		}
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- env.agentRepo.MergeConversationContext(env.ctx, fresh.ID, []agentdom.ContextItemRef{ref(agentdom.ContextItemTask, uuid.New())})
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			if e != nil {
				t.Fatal(e)
			}
		}
		if e := env.agentRepo.UpdateConversation(env.ctx, fresh); e != nil {
			t.Fatal(e)
		}
		stored, e := env.agentRepo.FindConversationByID(env.ctx, fresh.ID)
		if e != nil || stored.StartedAt == nil || len(stored.ContextItems) != 24 {
			t.Fatalf("lost cumulative state: %+v %v", stored, e)
		}
		session, _ := env.agentRepo.FindChatSessionByID(env.ctx, *fresh.ChatSessionID)
		session.MemberID = managerMember
		session.AgentID = uuid.New()
		session.ProjectID = uuid.New()
		if e := env.agentRepo.UpdateChatSession(env.ctx, session); e != nil {
			t.Fatal(e)
		}
		session, _ = env.agentRepo.FindChatSessionByID(env.ctx, *fresh.ChatSessionID)
		if session.MemberID != member || session.AgentID != aid || session.ProjectID != pid {
			t.Fatal("session identity overwritten")
		}
	})
}

func TestPrivateRunInternalHTTPAndGlobalIdentity(t *testing.T) {
	env := newE2EEnv(t)
	client, token, project, role := seedACPUser(t, env)
	pid := uuid.MustParse(project)
	_, result := createAgentRequest(t, env, client, token, project, acpAgentBody(role, "internal", nil))
	aid := uuid.MustParse(assertDataMap(t, result)["id"].(string))
	var member, user uuid.UUID
	if e := env.db.QueryRow(`SELECT id,user_id FROM project_members WHERE project_id=$1 AND member_type='human'`, pid).Scan(&member, &user); e != nil {
		t.Fatal(e)
	}
	instance := uuid.New()
	env.agentSvc.WithAuthorizer(authz.NewAuthorizer(pgRepo.NewAuthzPermissionStore(env.db))).WithPrivateRuns(instance, env.agentRepo, nil, nil, env.automationSvc)
	_, c, e := env.agentSvc.StartChatSession(authz.WithActorID(env.ctx, user), pid, aid, member, "first", nil, nil, nil, "")
	if e != nil {
		t.Fatal(e)
	}
	h := &handler.AgentRunHandler{Service: env.agentSvc, InternalKey: "synthetic-internal"}
	request := func(body, key string, want int) map[string]any {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/internal/agent-runs/authorize", strings.NewReader(body))
		r.Header.Set("X-Internal-Token", key)
		w := httptest.NewRecorder()
		h.Authorize(w, r)
		if w.Code != want {
			t.Fatalf("status=%d want=%d body=%s", w.Code, want, w.Body.String())
		}
		var result map[string]any
		if want == 200 {
			if json.Unmarshal(w.Body.Bytes(), &result) != nil {
				t.Fatal("bad response")
			}
			return result["data"].(map[string]any)
		}
		return nil
	}
	base := fmt.Sprintf(`{"conversation_id":%q,"agent_id":%q,"begin_turn":true}`, c.ID, aid)
	for _, key := range []string{"", "wrong"} {
		request(base, key, 401)
	}
	for _, body := range []string{`{}`, strings.Replace(base, `"begin_turn":true`, `"begin_turn":null`, 1), strings.Replace(base, `"begin_turn":true`, `"begin_turn":"true"`, 1), strings.Replace(base, `"begin_turn":true`, `"begin_turn":true,"owner_user_id":"spoof"`, 1), base + `{}`, strings.Replace(base, c.ID.String(), "invalid", 1)} {
		request(body, "synthetic-internal", 400)
	}
	first := request(base, "synthetic-internal", 200)
	if first["has_prior_turns"] != false || first["owner_user_id"] != user.String() || first["instance_id"] != instance.String() || len(first) != 11 {
		t.Fatalf("bad identity %+v", first)
	}
	if next := request(base, "synthetic-internal", 200); next["has_prior_turns"] != true {
		t.Fatal("prior latch not monotonic")
	}
	request(strings.Replace(base, aid.String(), uuid.NewString(), 1), "synthetic-internal", 404)
	if _, e = env.db.Exec(`UPDATE agent_conversations SET status='stopped' WHERE id=$1`, c.ID); e != nil {
		t.Fatal(e)
	}
	var before time.Time
	if e = env.db.Get(&before, `SELECT updated_at FROM agent_conversations WHERE id=$1`, c.ID); e != nil {
		t.Fatal(e)
	}
	request(base, "synthetic-internal", 404)
	var after time.Time
	if e = env.db.Get(&after, `SELECT updated_at FROM agent_conversations WHERE id=$1`, c.ID); e != nil {
		t.Fatal(e)
	}
	if !before.Equal(after) {
		t.Fatal("denied begin updated conversation")
	}
	if _, e = env.db.Exec(`UPDATE agent_conversations SET started_at=NULL WHERE id=$1`, c.ID); e != nil {
		t.Fatal(e)
	}
	var started *time.Time
	if e = env.db.Get(&started, `SELECT started_at FROM agent_conversations WHERE id=$1`, c.ID); e != nil {
		t.Fatal(e)
	}
	if started != nil {
		t.Fatal("denied begin changed latch")
	}
	global := uuid.New()
	if _, e = env.db.Exec(`INSERT INTO agents(id,name,handle,agent_type,agent_scope,acp_provider,llm_provider,llm_model,llm_api_key_secret) VALUES($1,'Global','global-private','acp','global','claude-code','','','')`, global); e != nil {
		t.Fatal(e)
	}
	session, g, e := env.agentSvc.StartGlobalChatSession(authz.WithActorID(env.ctx, user), global, user, "global", nil, "")
	if e != nil {
		t.Fatal(e)
	}
	a, e := env.agentSvc.AuthorizeRun(env.ctx, g.ID, global, true)
	if e != nil || a.ProjectID != "" || a.OwnerUserID != user.String() || a.ChatSessionID != session.ID.String() {
		t.Fatalf("global identity %+v %v", a, e)
	}
	if _, e = env.db.Exec(`UPDATE agent_conversations SET status='finished' WHERE id=$1`, g.ID); e != nil {
		t.Fatal(e)
	}
	if e = env.agentSvc.SendGlobalConversationMessage(authz.WithActorID(env.ctx, user), g.ID, "resume", user, nil, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = env.db.Exec(`UPDATE agent_conversations SET status='finished' WHERE id=$1`, g.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = env.agentSvc.SendGlobalChatMessage(authz.WithActorID(env.ctx, user), session.ID, user, "resume session", nil, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = env.agentSvc.GetGlobalConversation(env.ctx, g.ID, uuid.New()); e == nil {
		t.Fatal("other user read global")
	}
}

type privateFixRepo struct {
	*pgRepo.AgentRepository
	loseClaim    bool
	refuseOwner  func()
	mergeFailure func() error
}

func (r *privateFixRepo) ClaimConversationStatus(ctx context.Context, id uuid.UUID, from, to string) (bool, error) {
	if r.loseClaim {
		return false, nil
	}
	return r.AgentRepository.ClaimConversationStatus(ctx, id, from, to)
}
func (r *privateFixRepo) MergeConversationContext(ctx context.Context, id uuid.UUID, refs []agentdom.ContextItemRef) error {
	if r.mergeFailure != nil {
		return r.mergeFailure()
	}
	return r.AgentRepository.MergeConversationContext(ctx, id, refs)
}
func (r *privateFixRepo) CurrentChatActor(ctx context.Context, p, m, u uuid.UUID) (uuid.UUID, error) {
	if r.refuseOwner != nil {
		hook := r.refuseOwner
		r.refuseOwner = nil
		hook()
		return uuid.Nil, agentdom.ErrConversationNotFound
	}
	return r.AgentRepository.CurrentChatActor(ctx, p, m, u)
}

func TestPrivateFix1AcceptanceAndTerminalPreservation(t *testing.T) {
	env := newE2EEnv(t)
	client, token, project, role := seedACPUser(t, env)
	pid := uuid.MustParse(project)
	_, result := createAgentRequest(t, env, client, token, project, acpAgentBody(role, "fix1", nil))
	aid := uuid.MustParse(assertDataMap(t, result)["id"].(string))
	var member, user uuid.UUID
	if e := env.db.QueryRow(`SELECT id,user_id FROM project_members WHERE project_id=$1 AND member_type='human'`, pid).Scan(&member, &user); e != nil {
		t.Fatal(e)
	}
	ctx := authz.WithActorID(env.ctx, user)
	instance := uuid.New()
	repo := &privateFixRepo{AgentRepository: env.agentRepo}
	publisher := messaging.NewPublisher(env.redisClient, slog.Default())
	svc := agentsvc.New(repo, nil, publisher, nil).WithTaskAccess(env.taskSvc, env.taskSvc).WithAuthorizer(authz.NewAuthorizer(pgRepo.NewAuthzPermissionStore(env.db)))
	docs := docsvc.New(pgRepo.NewDocumentRepository(env.db), env.projectRepo)
	svc.WithPrivateRuns(instance, repo, docs, nil, env.automationSvc)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := env.db.Exec(q, args...); e != nil {
			t.Fatal(e)
		}
	}
	session, c, e := svc.StartChatSession(ctx, pid, aid, member, "start", nil, nil, nil, "")
	if e != nil {
		t.Fatal(e)
	}
	global := uuid.New()
	exec(`INSERT INTO agents(id,name,handle,agent_type,agent_scope,acp_provider,llm_provider,llm_model,llm_api_key_secret) VALUES($1,'Global','global-fix1','acp','global','claude-code','','','')`, global)
	gs, g, e := svc.StartGlobalChatSession(ctx, global, user, "global", nil, "")
	if e != nil {
		t.Fatal(e)
	}
	doc := uuid.New()
	exec(`INSERT INTO documents(id,project_id,title) VALUES($1,$2,'accepted document')`, doc, pid)
	p := pid.String()
	refs := []agentdom.ContextItemRef{{Type: agentdom.ContextItemDoc, ID: doc.String(), ProjectID: &p}}
	var onBusy string
	calls := []struct {
		name string
		c    *agentdom.AgentConversation
		send func() error
	}{
		{"project_conversation", c, func() error { return svc.SendConversationMessage(ctx, pid, c.ID, "new", member, refs, onBusy) }},
		{"project_session", c, func() error { _, e := svc.SendChatMessage(ctx, pid, session.ID, member, "new", refs, onBusy); return e }},
		{"global_conversation", g, func() error { return svc.SendGlobalConversationMessage(ctx, g.ID, "new", user, refs, onBusy) }},
		{"global_session", g, func() error { _, e := svc.SendGlobalChatMessage(ctx, gs.ID, user, "new", refs, onBusy); return e }},
	}
	t.Run("rejected_busy_capacity_and_claim_are_read_only", func(t *testing.T) {
		for _, call := range calls {
			t.Run(call.name, func(t *testing.T) {
				for _, state := range []string{"running", "queued", "lost_claim", "capacity"} {
					status := state
					if state == "lost_claim" || state == "capacity" {
						status = "finished"
					}
					exec(`UPDATE agent_conversations SET status=$1,context_items='[]' WHERE id=$2`, status, call.c.ID)
					repo.loseClaim = state == "lost_claim"
					var blocker *agentdom.AgentConversation
					if state == "capacity" {
						blocker = &agentdom.AgentConversation{ID: uuid.New(), AgentID: call.c.AgentID, ProjectID: call.c.ProjectID, TriggerType: "chat_message", Status: "running", ContextItems: []agentdom.ContextItemRef{}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
						if e := env.agentRepo.CreateConversation(ctx, blocker); e != nil {
							t.Fatal(e)
						}
					}
					if e := call.send(); e == nil {
						t.Errorf("%s accepted refusal case", state)
					}
					stored, e := env.agentRepo.FindConversationByID(ctx, call.c.ID)
					if e != nil {
						t.Fatal(e)
					}
					if len(stored.ContextItems) != 0 {
						t.Errorf("%s persisted rejected refs: %+v", state, stored.ContextItems)
					}
					if blocker != nil {
						exec(`UPDATE agent_conversations SET status='finished' WHERE id=$1`, blocker.ID)
					}
					repo.loseClaim = false
				}
				exec(`UPDATE agent_conversations SET status='finished',context_items='[]' WHERE id=$1`, call.c.ID)
				if e := call.send(); e != nil {
					t.Fatal(e)
				}
				stored, _ := env.agentRepo.FindConversationByID(ctx, call.c.ID)
				if len(stored.ContextItems) != 1 || stored.ContextItems[0].ID != doc.String() {
					t.Fatal("accepted continuation did not persist refs")
				}
				exec(`UPDATE agent_conversations SET status='finished' WHERE id=$1`, call.c.ID)
			})
		}
	})
	t.Run("fix2_merge_failure_restores_acceptance", func(t *testing.T) {
		failure := errors.New("injected context persistence unavailable")
		for _, call := range calls {
			for _, queued := range []bool{false, true} {
				onBusy = ""
				prior := "finished"
				var blocker *agentdom.AgentConversation
				if queued {
					onBusy = "queue"
					prior = "paused"
					blocker = &agentdom.AgentConversation{ID: uuid.New(), AgentID: call.c.AgentID, ProjectID: call.c.ProjectID, TriggerType: "chat_message", Status: "running", CreatedAt: time.Now(), UpdatedAt: time.Now()}
					if e := env.agentRepo.CreateConversation(ctx, blocker); e != nil {
						t.Fatal(e)
					}
				}
				exec(`UPDATE agent_conversations SET status=$1,context_items='[]' WHERE id=$2`, prior, call.c.ID)
				before, e := env.redisClient.XLen(ctx, events.StreamAgentTriggers).Result()
				if e != nil {
					t.Fatal(e)
				}
				repo.mergeFailure = func() error { return failure }
				if e := call.send(); !errors.Is(e, failure) {
					t.Errorf("%s queued=%v returned %v", call.name, queued, e)
				}
				repo.mergeFailure = nil
				stored, e := env.agentRepo.FindConversationByID(ctx, call.c.ID)
				if e != nil {
					t.Fatal(e)
				}
				if stored.Status != prior || len(stored.ContextItems) != 0 {
					t.Errorf("%s queued=%v orphan status=%s refs=%v", call.name, queued, stored.Status, stored.ContextItems)
				}
				after, e := env.redisClient.XLen(ctx, events.StreamAgentTriggers).Result()
				if e != nil {
					t.Fatal(e)
				}
				var pending int
				if e := env.db.Get(&pending, `SELECT count(*) FROM agent_pending_triggers WHERE conversation_id=$1`, call.c.ID); e != nil {
					t.Fatal(e)
				}
				if before != after || pending != 0 {
					t.Errorf("%s queued=%v dispatched unpersisted refs", call.name, queued)
				}
				exec(`UPDATE agent_conversations SET status='finished' WHERE id=$1`, call.c.ID)
				if blocker != nil {
					exec(`UPDATE agent_conversations SET status='finished' WHERE id=$1`, blocker.ID)
				}
			}
		}
		onBusy = ""
		for _, terminal := range []string{"stopped", "finished", "failed", "canceled"} {
			exec(`UPDATE agent_conversations SET status='paused',context_items='[]' WHERE id=$1`, c.ID)
			parentCtx := ctx
			cancel := func() {}
			if terminal == "canceled" {
				ctx, cancel = context.WithCancel(parentCtx)
			}
			repo.mergeFailure = func() error {
				if terminal == "canceled" {
					cancel()
					return ctx.Err()
				}
				exec(`UPDATE agent_conversations SET status=$1 WHERE id=$2`, terminal, c.ID)
				return failure
			}
			if e := calls[0].send(); e == nil {
				t.Fatal("injected merge failure accepted")
			}
			ctx = parentCtx
			cancel()
			repo.mergeFailure = nil
			stored, e := env.agentRepo.FindConversationByID(ctx, c.ID)
			if e != nil {
				t.Fatal(e)
			}
			expected := terminal
			if terminal == "canceled" {
				expected = "paused"
			}
			if stored.Status != expected {
				t.Errorf("merge failure replaced %s with %s", expected, stored.Status)
			}
		}
		// A successful retry still persists before dispatch after rollback.
		exec(`UPDATE agent_conversations SET status='finished' WHERE id=$1`, c.ID)
		if e := calls[0].send(); e != nil {
			t.Fatal(e)
		}
		stored, e := env.agentRepo.FindConversationByID(ctx, c.ID)
		if e != nil {
			t.Fatal(e)
		}
		if len(stored.ContextItems) != 1 {
			t.Fatal("successful retry lost refs")
		}
		exec(`UPDATE agent_conversations SET status='finished' WHERE id=$1`, c.ID)
	})
	t.Run("queue_refusal_preserves_concurrent_terminal_state", func(t *testing.T) {
		for _, terminal := range []string{"stopped", "finished", "failed", "queued"} {
			exec(`UPDATE agent_conversations SET status='queued',context_items='[]' WHERE id=$1`, c.ID)
			if e := env.agentRepo.CreatePendingTrigger(ctx, &agentdom.PendingTrigger{ID: uuid.New(), AgentID: aid, ConversationID: c.ID, Topic: events.TopicAgentChatMessage, Payload: map[string]string{}, CreatedAt: time.Now()}); e != nil {
				t.Fatal(e)
			}
			repo.refuseOwner = func() { exec(`UPDATE agent_conversations SET status=$1 WHERE id=$2`, terminal, c.ID) }
			before, _ := env.redisClient.XLen(ctx, events.StreamAgentConversationStatus).Result()
			if _, e := svc.AdvanceQueue(ctx, aid, 1); e != nil {
				t.Fatal(e)
			}
			stored, _ := env.agentRepo.FindConversationByID(ctx, c.ID)
			expected := terminal
			if terminal == "queued" {
				expected = "failed"
			}
			if stored.Status != expected {
				t.Errorf("concurrent %s became %s, want %s", terminal, stored.Status, expected)
			}
			after, _ := env.redisClient.XLen(ctx, events.StreamAgentConversationStatus).Result()
			expectedEvents := before
			if terminal == "queued" {
				expectedEvents++
			}
			if after != expectedEvents {
				t.Error("refusal emitted false/duplicate terminal status or missed queued failure")
			}
			var n int
			env.db.Get(&n, `SELECT count(*) FROM agent_pending_triggers WHERE conversation_id=$1`, c.ID)
			if n != 0 {
				t.Fatal("pending not removed")
			}
		}
	})
	t.Run("opaque_internal_refusal_and_dependency_failure", func(t *testing.T) {
		h := &handler.AgentRunHandler{Service: svc, InternalKey: "synthetic"}
		request := func() *httptest.ResponseRecorder {
			r := httptest.NewRequest("POST", "/", strings.NewReader(fmt.Sprintf(`{"conversation_id":%q,"agent_id":%q,"begin_turn":false}`, c.ID, aid)))
			r.Header.Set("X-Internal-Token", "synthetic")
			w := httptest.NewRecorder()
			h.Authorize(w, r)
			return w
		}
		var bodies []string
		for _, kind := range []string{"task", "doc", "conversation"} {
			b, _ := json.Marshal([]agentdom.ContextItemRef{{Type: agentdom.ContextItemType(kind), ID: uuid.NewString(), ProjectID: &p}})
			exec(`UPDATE agent_conversations SET context_items=$1 WHERE id=$2`, b, c.ID)
			w := request()
			if w.Code != 404 {
				t.Errorf("%s status=%d", kind, w.Code)
			}
			bodies = append(bodies, w.Body.String())
		}
		if bodies[0] != bodies[1] || bodies[1] != bodies[2] {
			t.Errorf("object-specific refusal bodies: %v", bodies)
		}
		b, _ := json.Marshal(refs)
		exec(`UPDATE agent_conversations SET context_items=$1 WHERE id=$2`, b, c.ID)
		exec(`ALTER TABLE documents RENAME TO documents_temporarily_unavailable`)
		w := request()
		exec(`ALTER TABLE documents_temporarily_unavailable RENAME TO documents`)
		if w.Code != 500 {
			t.Errorf("dependency failure status=%d", w.Code)
		}
	})
	t.Run("legacy_shared_queued_attachments_survive_without_private_certification", func(t *testing.T) {
		svc.WithPrivateRuns(uuid.Nil, repo, docs, nil, env.automationSvc)
		shared := &agentdom.AgentConversation{ID: uuid.New(), AgentID: aid, ProjectID: pid, TriggerType: "automation_message", Status: "finished", CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if e := env.agentRepo.CreateConversation(ctx, shared); e != nil {
			t.Fatal(e)
		}
		exec(`UPDATE agent_conversations SET status='running' WHERE id=$1`, c.ID)
		if e := svc.SendConversationMessage(ctx, pid, shared.ID, "legacy", member, refs, "queue"); e != nil {
			t.Fatal(e)
		}
		exec(`UPDATE agent_conversations SET status='finished' WHERE id=$1`, c.ID)
		if _, e := svc.AdvanceQueue(ctx, aid, 1); e != nil {
			t.Fatal(e)
		}
		messages, e := env.redisClient.XRevRangeN(ctx, events.StreamAgentTriggers, "+", "-", 1).Result()
		if e != nil || len(messages) != 1 {
			t.Fatal("missing legacy trigger")
		}
		raw, _ := messages[0].Values["context_items"].(string)
		if !strings.Contains(raw, doc.String()) || !strings.Contains(raw, "accepted document") {
			t.Errorf("legacy context lost: %q", raw)
		}
		stored, _ := env.agentRepo.FindConversationByID(ctx, shared.ID)
		if stored.ContextItems != nil {
			t.Fatal("legacy unknown history certified")
		}
	})
}
