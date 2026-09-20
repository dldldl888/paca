package e2e_test

import (
	"encoding/json"
	"fmt"
	"github.com/Paca-AI/api/internal/transport/http/handler"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	agentsvc "github.com/Paca-AI/api/internal/service/agent"
	docsvc "github.com/Paca-AI/api/internal/service/doc"
	"github.com/google/uuid"
)

// A hidden row ahead of the cursor/limit must neither crowd out allowed rows
// nor disclose its historical text. Reference bodies retain the target identity.
func TestDocumentConsumersCurrentScope(t *testing.T) {
	env := newE2EEnv(t)
	client, token, project, role := seedACPUser(t, env)
	pid := uuid.MustParse(project)
	_, result := createAgentRequest(t, env, client, token, project, acpAgentBody(role, "doc-consumers", nil))
	aid := uuid.MustParse(assertDataMap(t, result)["id"].(string))
	_, resultB := createAgentRequest(t, env, client, token, project, acpAgentBody(role, "doc-consumers-b", nil))
	bid := uuid.MustParse(assertDataMap(t, resultB)["id"].(string))
	var manager, agentMember uuid.UUID
	if err := env.db.Get(&manager, `SELECT user_id FROM project_members WHERE project_id=$1 AND member_type='human' LIMIT 1`, pid); err != nil {
		t.Fatal(err)
	}
	if err := env.db.Get(&agentMember, `SELECT id FROM project_members WHERE project_id=$1 AND agent_id=$2`, pid, aid); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	var users, members [2]uuid.UUID
	for i, name := range []string{"consumer-a", "consumer-b"} {
		seedUser(t, env, name, "password1", name)
		u, e := env.userRepo.FindByUsername(env.ctx, name)
		if e != nil {
			t.Fatal(e)
		}
		users[i] = u.ID
		members[i] = uuid.New()
		exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type)VALUES($1,$2,$3,$4,'human')`, members[i], pid, u.ID, role)
	}
	ctx := authz.WithActorID(env.ctx, users[0])
	managerCtx := authz.WithActorID(env.ctx, manager)
	taskA, e := env.taskSvc.CreateTask(managerCtx, taskdom.CreateTaskInput{ProjectID: pid, Title: "A task", AssigneeIDs: []uuid.UUID{members[0]}})
	if e != nil {
		t.Fatal(e)
	}
	taskB, e := env.taskSvc.CreateTask(managerCtx, taskdom.CreateTaskInput{ProjectID: pid, Title: "B task", AssigneeIDs: []uuid.UUID{members[1]}})
	if e != nil {
		t.Fatal(e)
	}
	folder := uuid.New()
	exec(`INSERT INTO doc_folders(id,project_id,name)VALUES($1,$2,'公司资料')`, folder, pid)
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	add := func(visibility, title string, owner uuid.UUID, task, source *uuid.UUID, at time.Time) uuid.UUID {
		id := uuid.New()
		var content any = `[{"content":[{"type":"text","text":"` + title + ` body"}]}]`
		if source != nil {
			content = nil
		}
		exec(`INSERT INTO documents(id,project_id,folder_id,title,content,visibility,owner_user_id,source_document_id,updated_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, pid, folder, title, content, visibility, owner, source, at)
		if task != nil {
			exec(`INSERT INTO document_tasks(document_id,task_id)VALUES($1,$2)`, id, *task)
		}
		exec(`INSERT INTO doc_activities(id,document_id,actor_id,activity_type,content,created_at)VALUES($1,$2,$3,'doc.comment.added',$4,$5)`, uuid.New(), id, agentMember, `{"text":"`+title+` history"}`, at)
		return id
	}
	own := add("private", "own", users[0], nil, nil, stamp)
	taskDoc := add("task", "task-A", manager, &taskA.ID, nil, stamp.Add(time.Second))
	add("project", "shared", manager, nil, nil, stamp.Add(2*time.Second))
	legacy := add("legacy_project", "legacy", manager, nil, nil, stamp.Add(3*time.Second))
	sourceProject := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type)SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, uuid.New(), sourceProject, users[0])
	source := uuid.New()
	exec(`INSERT INTO documents(id,project_id,title,content,visibility,owner_user_id,updated_at)VALUES($1,$2,'canonical','[{"content":[{"type":"text","text":"CANONICAL_BODY"}]}]','project',$3,$4)`, source, sourceProject, manager, stamp.Add(-time.Hour))
	reference := add("project", "reference", manager, nil, &source, stamp.Add(4*time.Second))
	hidden := []uuid.UUID{}
	for i := 0; i < 24; i++ {
		hidden = append(hidden, add("private", fmt.Sprintf("HIDDEN-%02d", i), users[1], nil, nil, stamp.Add(time.Duration(100+i)*time.Second)))
	}
	hidden = append(hidden, add("private", "HIDDEN-manager", manager, nil, nil, stamp.Add(200*time.Second)))
	hidden = append(hidden, add("task", "HIDDEN-task-B", manager, &taskB.ID, nil, stamp.Add(201*time.Second)))
	t.Run("company_filter_before_limit_and_source_projection", func(t *testing.T) {
		docs, err := env.agentRepo.ListCompanyDocuments(ctx, []uuid.UUID{pid}, 21)
		if err != nil {
			t.Fatal(err)
		}
		if len(docs) != 5 {
			t.Fatalf("authorized company set got=%d want=5 (hidden rows must not occupy LIMIT21)", len(docs))
		}
		for _, d := range docs {
			if strings.Contains(d.Title, "HIDDEN") {
				t.Fatal("hidden company row", d.ID)
			}
			if d.ID == reference {
				if !strings.Contains(string(d.Content), "CANONICAL_BODY") || d.SourceUpdatedAt == nil || !d.SourceUpdatedAt.Equal(stamp.Add(-time.Hour)) || d.SourceDocumentID == nil || *d.SourceDocumentID != source {
					t.Fatalf("reference missing canonical projection: %+v", d)
				}
			}
		}
	})
	t.Run("activity_search_and_two_pages", func(t *testing.T) {
		filter := agentdom.ListAgentActivitiesFilter{ProjectID: pid, ActorMemberID: agentMember}
		page, more, err := env.agentSvc.ListAgentActivities(ctx, filter, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 3 || !more {
			t.Fatalf("first page %d more=%v", len(page), more)
		}
		cursor := agentdom.EncodeActivityFeedCursor(page[2])
		filter.CursorAfter = &cursor
		second, more, err := env.agentSvc.ListAgentActivities(ctx, filter, 3)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(append(page, second...))
		if len(second) != 2 || more || strings.Contains(string(raw), "HIDDEN") {
			t.Fatalf("hidden activity consumed pages: second=%d more=%v data=%s", len(second), more, raw)
		}
		q := "HIDDEN"
		filter.CursorAfter = nil
		filter.Search = &q
		page, more, err = env.agentSvc.ListAgentActivities(ctx, filter, 1)
		if err != nil || len(page) != 0 || more {
			t.Fatalf("hidden activity search leaked: %+v more=%v err=%v", page, more, err)
		}
	})
	coordination := startFileDelivery(t, env)
	coordination.Reconcile = env.agentSvc.ReconcilePrivateRuns
	env.agentSvc.WithAuthorizer(authz.NewAuthorizer(pg.NewAuthzPermissionStore(env.db))).WithPrivateRuns(uuid.New(), env.agentRepo, docsvc.New(pg.NewDocumentRepository(env.db), nil), nil, nil).WithDeliveryGate(env.fileGate)
	if err := coordination.Mutate(env.ctx, nil); err != nil {
		t.Fatal(err)
	}
	waitFileGate(t, env)
	t.Run("current_document_recipients", func(t *testing.T) {
		for _, tc := range []struct {
			kind, id, project string
			wantA, wantB      bool
		}{
			{"doc.updated", own.String(), project, true, false}, {"doc.created", taskDoc.String(), project, true, false}, {"doc.comment.added", reference.String(), project, true, false},
			{"doc.updated", "", project, false, false}, {"doc.updated", own.String(), sourceProject.String(), false, false}, {"doc.comment.updated", hidden[0].String(), project, false, true},
		} {
			var scope agentsvc.RealtimeScope
			raw, _ := json.Marshal(map[string]any{"event_type": tc.kind, "project_id": tc.project, "document_id": tc.id, "activity_id": uuid.NewString(), "activity_type": "doc.comment.added", "content": "{\"text\":\"secret\"}"})
			_ = json.Unmarshal(raw, &scope)
			recipients, err := env.agentSvc.CurrentRecipients(env.ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			has := func(u uuid.UUID) bool {
				for _, id := range recipients.UserIDs {
					if id == u.String() {
						return true
					}
				}
				return false
			}
			if has(users[0]) != tc.wantA || has(users[1]) != tc.wantB {
				t.Errorf("%s doc=%s recipients A=%v B=%v want A=%v B=%v", tc.kind, tc.id, has(users[0]), has(users[1]), tc.wantA, tc.wantB)
			}
		}
	})
	t.Run("legacy_global_projects_reader_without_docs_permission", func(t *testing.T) {
		seedUser(t, env, "consumer-legacy", "password1", "Legacy")
		u, err := env.userRepo.FindByUsername(env.ctx, "consumer-legacy")
		if err != nil {
			t.Fatal(err)
		}
		gr := uuid.New()
		exec(`INSERT INTO global_roles(id,name,permissions)VALUES($1,'consumer-legacy','["projects.read"]')`, gr)
		if err = env.roleRepo.ReplaceUserRoles(env.ctx, u.ID, []uuid.UUID{gr}); err != nil {
			t.Fatal(err)
		}
		waitFileGate(t, env)
		canRead, err := authz.NewAuthorizer(pg.NewAuthzPermissionStore(env.db)).HasPermissions(authz.WithActorID(env.ctx, u.ID), u.ID, &pid, "", authz.PermissionDocsRead)
		if err != nil || canRead {
			t.Fatal("fixture unexpectedly has docs.read", canRead, err)
		}
		for _, tc := range []struct {
			id   uuid.UUID
			want bool
		}{{legacy, true}, {own, false}} {
			var scope agentsvc.RealtimeScope
			raw, _ := json.Marshal(map[string]any{"event_type": "doc.updated", "project_id": pid, "document_id": tc.id})
			_ = json.Unmarshal(raw, &scope)
			recipients, err := env.agentSvc.CurrentRecipients(env.ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, id := range recipients.UserIDs {
				if id == u.ID.String() {
					found = true
				}
			}
			if found != tc.want {
				t.Fatalf("legacy reader doc=%s found=%v want=%v", tc.id, found, tc.want)
			}
		}
	})
	t.Run("real_company_API_durable_reference_source_and_target_revocation", func(t *testing.T) {
		internal := httptest.NewServer(http.HandlerFunc((&handler.AgentRunHandler{Service: env.agentSvc, InternalKey: "synthetic-company"}).CompanyContext))
		defer internal.Close()
		start := func(i int) *agentdom.AgentConversation {
			t.Helper()
			waitFileGate(t, env)
			targetAgent := aid
			if i == 1 {
				targetAgent = bid
			}
			_, c, err := env.agentSvc.StartChatSession(authz.WithActorID(env.ctx, users[i]), pid, targetAgent, members[i], "company", nil, nil, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
		company := func(c *agentdom.AgentConversation, want int) *agentsvc.CompanyContext {
			t.Helper()
			waitFileGate(t, env)
			req := mustRequest(env.ctx, t, "POST", internal.URL, jsonBody(t, map[string]any{"conversation_id": c.ID, "agent_id": c.AgentID}))
			req.Header.Set("X-Internal-Token", "synthetic-company")
			resp := mustDo(t, client, req)
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != want {
				t.Fatalf("company API got=%d want=%d body=%s", resp.StatusCode, want, raw)
			}
			if want != 200 {
				return nil
			}
			var response struct {
				Data *agentsvc.CompanyContext `json:"data"`
			}
			if err := json.Unmarshal(raw, &response); err != nil {
				t.Fatal(err)
			}
			return response.Data
		}
		c := start(0)
		out := company(c, 200)
		if len(out.Sources) != 5 || out.Truncated || strings.Contains(out.Text, "HIDDEN") || !strings.Contains(out.Text, "CANONICAL_BODY") || !strings.Contains(out.Text, source.String()) {
			t.Fatalf("company A: %+v", out)
		}
		if out.Sources[0].DocumentID != reference.String() || out.Sources[0].UpdatedAt != "2026-01-02T02:04:05Z" {
			t.Fatalf("reference identity/time %+v", out.Sources[0])
		}
		stored, err := env.agentRepo.FindConversationByID(env.ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, ref := range stored.ContextItems {
			if ref.ID == reference.String() {
				found = true
			}
		}
		if !found {
			t.Fatal("reference not durable", stored.ContextItems)
		}
		b := company(start(1), 200)
		if len(b.Sources) != 20 || !b.Truncated || strings.Contains(b.Text, "CANONICAL_BODY") || strings.Contains(b.Text, "task-A body") {
			t.Fatalf("company B: %+v", b)
		}
		revoke := func(p, d uuid.UUID) {
			t.Helper()
			waitFileGate(t, env)
			req := mustRequest(env.ctx, t, "PATCH", fmt.Sprintf("%s/api/v1/projects/%s/docs/%s/access", env.base, p, d), jsonBody(t, map[string]any{"visibility": "private", "expected_access_version": 1}))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			resp := mustDo(t, client, req)
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("revoke %d %s", resp.StatusCode, raw)
			}
		}
		revoke(sourceProject, source)
		company(c, 404)
		next := start(0)
		out = company(next, 200)
		if len(out.Sources) != 4 || strings.Contains(out.Text, "CANONICAL_BODY") || strings.Contains(out.Text, reference.String()) {
			t.Fatal("source revocation missing", out)
		}
		revoke(pid, taskDoc)
		company(next, 404)
		out = company(start(0), 200)
		if len(out.Sources) != 3 || strings.Contains(out.Text, "task-A body") {
			t.Fatal("target revocation missing", out)
		}
	})
	t.Run("activity_real_HTTP_search_cursor_after_revocation", func(t *testing.T) {
		aClient, aToken := taskMemberLogin(t, env, "consumer-a", "password1")
		read := func(query string) map[string]any {
			t.Helper()
			req := mustRequest(env.ctx, t, "GET", fmt.Sprintf("%s/api/v1/projects/%s/agents/%s/activities?%s", env.base, pid, aid, query), nil)
			req.Header.Set("Authorization", "Bearer "+aToken)
			resp := mustDo(t, aClient, req)
			defer resp.Body.Close()
			assertStatus(t, resp, 200)
			var result envelope
			decodeJSON(t, resp, &result)
			return assertDataMap(t, result)
		}
		first := read("page_size=2")
		if len(first["items"].([]any)) != 2 || first["next_cursor"] == nil {
			t.Fatal(first)
		}
		second := read("page_size=2&cursor=" + url.QueryEscape(first["next_cursor"].(string)))
		raw, _ := json.Marshal([]any{first, second})
		if len(second["items"].([]any)) != 1 || second["next_cursor"] != nil || strings.Contains(string(raw), "HIDDEN") || strings.Contains(string(raw), "reference") || strings.Contains(string(raw), "task-A") {
			t.Fatal("activity current scope", string(raw))
		}
		for _, search := range []string{"HIDDEN", "reference", "task-A"} {
			rows := read("page_size=1&search=" + search)
			if len(rows["items"].([]any)) != 0 || rows["next_cursor"] != nil {
				t.Fatal("hidden search", rows)
			}
		}
	})

}
