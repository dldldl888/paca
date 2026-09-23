package e2e_test

import (
	"reflect"
	"testing"

	draftdom "github.com/Paca-AI/api/internal/domain/draft"
	"github.com/Paca-AI/api/internal/platform/authz"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	draftsvc "github.com/Paca-AI/api/internal/service/draft"
	"github.com/google/uuid"
)

func newProjectDraftFixture(t *testing.T) (*draftFixture, uuid.UUID, uuid.UUID) {
	t.Helper()
	env := newE2EEnv(t)
	seedProjectAdminUser(t, env, "draft-owner", "password1")
	client, token := projectAdminLogin(t, env, "draft-owner", "password1")
	owner, err := env.userRepo.FindByUsername(env.ctx, "draft-owner")
	if err != nil {
		t.Fatal(err)
	}
	agentID := uuid.New()
	if _, err := env.db.Exec(`INSERT INTO agents(id,name,handle,agent_type,agent_scope,acp_provider,llm_provider,llm_model,llm_api_key_secret) VALUES($1,'Draft fixture','draft-fixture','acp','global','claude-code','','','')`, agentID); err != nil {
		t.Fatal(err)
	}
	session, _, err := env.agentSvc.StartGlobalChatSession(authz.WithActorID(env.ctx, owner.ID), agentID, owner.ID, "anchor", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return &draftFixture{env: env, client: client, token: token, base: "/api/v1/project-drafts"}, owner.ID, session.ID
}

func projectBusinessCounts(t *testing.T, env *e2eEnv) []int {
	t.Helper()
	counts := []int{}
	for _, table := range []string{"projects", "project_members", "tasks", "documents", "document_tasks", "notifications"} {
		var n int
		if err := env.db.Get(&n, "SELECT count(*) FROM "+table); err != nil {
			t.Fatal(err)
		}
		counts = append(counts, n)
	}
	return counts
}

func TestPrivateProjectDraftTaskCandidatesAndRemoval(t *testing.T) {
	f, owner, session := newProjectDraftFixture(t)
	before := projectBusinessCounts(t, f.env)
	itemID := uuid.NewString()
	body := map[string]any{
		"name": "筹备", "goal": "私人目标", "chat_session_id": session,
		"items": []any{map[string]any{
			"id": itemID, "title": "待分派任务", "requirements": "任务要求", "criteria": "验收标准",
			"assignee_user_id": owner.String(), "due_date": "2026-12-01T18:00:00+08:00", "importance": 2,
			"dependencies": []any{},
		}},
		"participants": []any{map[string]any{"user_id": owner.String(), "role": "候选执行人"}},
		"materials":    []any{map[string]any{"id": uuid.NewString(), "kind": "document", "task_ids": []any{itemID}}},
	}
	d := f.request(t, "POST", f.base, body, 201)
	path := f.base + "/" + d["id"].(string)
	current := f.request(t, "GET", path, nil, 200)
	if current["name"] != d["name"] || current["goal"] != d["goal"] || current["items"] == nil || current["participants"] == nil || current["materials"] == nil {
		t.Fatalf("refresh lost draft content: create=%#v get=%#v", d, current)
	}
	if current["items"].([]any)[0].(map[string]any)["assignee_user_id"] != owner.String() {
		t.Fatal("candidate user was not retained")
	}
	delete(body, "chat_session_id")
	body["expected_version"] = 1
	body["items"], body["participants"], body["materials"] = []any{}, []any{}, []any{}
	d = f.request(t, "PATCH", path, body, 200)
	if d["version"] != float64(2) {
		t.Fatal("removal did not invalidate prior version")
	}
	for _, field := range []string{"items", "participants", "materials"} {
		if len(d[field].([]any)) != 0 {
			t.Fatalf("%s was not removed", field)
		}
	}
	f.request(t, "PATCH", path, body, 409)
	if after := projectBusinessCounts(t, f.env); !reflect.DeepEqual(before, after) {
		t.Fatalf("private edits created formal data: %v -> %v", before, after)
	}
}

func TestPrivateProjectDraftInvalidContentIsBadRequest(t *testing.T) {
	f, _, session := newProjectDraftFixture(t)
	f.request(t, "POST", f.base, map[string]any{"name": " ", "chat_session_id": session}, 400)
	d := f.request(t, "POST", f.base, map[string]any{"name": "有效", "chat_session_id": session}, 201)
	path := f.base + "/" + d["id"].(string)
	f.request(t, "PATCH", path, map[string]any{"name": " ", "expected_version": 1}, 400)
	if got := f.request(t, "GET", path, nil, 200); got["version"] != float64(1) || got["name"] != "有效" {
		t.Fatal("invalid update modified draft")
	}
}

func TestPrivateProjectDraftServiceRechecksAuthorityAndSession(t *testing.T) {
	f, owner, session := newProjectDraftFixture(t)
	authorizer := authz.NewAuthorizer(pg.NewAuthzPermissionStore(f.env.db)).WithAgentRoleResolver(pg.NewAuthzPermissionStore(f.env.db))
	service := draftsvc.New(pg.NewDraftRepository(f.env.db), f.env.agentSvc, f.env.projectRepo).WithAuthorizer(authorizer)
	ctx := authz.WithHumanSession(authz.WithActorID(f.env.ctx, owner))
	content := draftdom.ProjectDraftContent{Name: "私人内容"}
	d, err := service.CreateProjectDraft(ctx, content, session)
	if err != nil {
		t.Fatal(err)
	}
	// A second authorized user cannot attach a draft to the owner's session.
	seedProjectAdminUser(t, f.env, "other-owner", "password1")
	other, err := f.env.userRepo.FindByUsername(f.env.ctx, "other-owner")
	if err != nil {
		t.Fatal(err)
	}
	otherCtx := authz.WithHumanSession(authz.WithActorID(f.env.ctx, other.ID))
	if _, err := service.CreateProjectDraft(otherCtx, content, session); err == nil {
		t.Errorf("foreign session accepted: %v", err)
	}
	// Reusing the same authenticated actor context must not reuse an old allow.
	assignGlobalRolesByName(t, f.env, "draft-owner", "USER")
	if _, err := service.GetProjectDraft(ctx, d.ID); err == nil {
		t.Error("revoked actor read a private draft")
	}
	if _, err := service.ListProjectDrafts(ctx); err == nil {
		t.Error("revoked actor listed private drafts")
	}
	if _, err := service.UpdateProjectDraft(ctx, d.ID, 1, content); err == nil {
		t.Error("revoked actor edited a private draft")
	}
	if _, err := service.CreateProjectDraft(ctx, content, session); err == nil {
		t.Error("revoked actor created a private draft")
	}
}
