package e2e_test

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	draftdom "github.com/Paca-AI/api/internal/domain/draft"
	"github.com/Paca-AI/api/internal/platform/authz"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	draftsvc "github.com/Paca-AI/api/internal/service/draft"
	"github.com/google/uuid"
)

func TestPrivateProjectDraftMaterialSearchOnlyReturnsReadableDocuments(t *testing.T) {
	f, _, session, _ := newProjectDraftFixture(t)
	projectID := createProjectForTasksViaAPI(t, f.env, f.client, f.token)
	base := f.env.base + "/api/v1/projects/" + projectID + "/docs"
	create := func(client *http.Client, token, title string) {
		req := mustRequest(f.env.ctx, t, http.MethodPost, base, jsonBody(t, map[string]any{
			"title":   title,
			"content": []any{},
		}))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp := mustDo(t, client, req)
		resp.Body.Close()
		assertStatus(t, resp, http.StatusCreated)
	}
	create(f.client, f.token, "可读路线图")
	seedUser(t, f.env, "draft-material-peer", "password1", "Peer")
	if _, err := f.env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, uuid.New(), projectID, func() uuid.UUID {
		u, err := f.env.userRepo.FindByUsername(f.env.ctx, "draft-material-peer")
		if err != nil {
			t.Fatal(err)
		}
		return u.ID
	}()); err != nil {
		t.Fatal(err)
	}
	peerClient, peerToken := taskMemberLogin(t, f.env, "draft-material-peer", "password1")
	create(peerClient, peerToken, "不可读路线图")

	f.request(t, http.MethodPost, f.base, map[string]any{
		"name": "资料候选", "goal": "检索资料", "chat_session_id": session,
	}, http.StatusCreated)
	result := f.request(t, http.MethodGet, f.base+"/materials?search=路线图&limit=20", nil, http.StatusOK)
	items, ok := result["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["title"] != "可读路线图" {
		t.Fatalf("material search leaked or omitted documents: %#v", result)
	}
	if items[0].(map[string]any)["project_id"] != projectID || items[0].(map[string]any)["source_type"] != "internal" {
		t.Fatalf("material metadata missing: %#v", items[0])
	}
}

func TestPrivateProjectDraftFileUploadTracksParsingAndOwnerIsolation(t *testing.T) {
	f, _, session, _ := newProjectDraftFixture(t)
	d := f.request(t, http.MethodPost, f.base, map[string]any{
		"name": "上传资料", "goal": "验证文件状态", "chat_session_id": session,
	}, http.StatusCreated)
	path := f.base + "/" + d["id"].(string)
	start := f.request(t, http.MethodPost, path+"/files/initiate-upload", map[string]any{
		"file_name": "notes.txt", "content_type": "text/plain", "file_size": 11,
	}, http.StatusCreated)
	if start["upload_url"] == nil || start["file_id"] == nil {
		t.Fatalf("missing upload session: %#v", start)
	}
	uploadReq, err := http.NewRequest(http.MethodPut, start["upload_url"].(string), strings.NewReader("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	uploadReq.Header.Set("Content-Type", "text/plain")
	uploadResp, err := f.client.Do(uploadReq)
	if err != nil {
		t.Fatal(err)
	}
	uploadResp.Body.Close()
	assertStatus(t, uploadResp, http.StatusOK)
	completed := f.request(t, http.MethodPost, path+"/files/complete-upload", map[string]any{
		"file_id": start["file_id"],
	}, http.StatusOK)
	if completed["processing_status"] != "ready" || completed["upload_status"] != "uploaded" {
		t.Fatalf("text file was not parsed: %#v", completed)
	}
	unsupported := f.request(t, http.MethodPost, path+"/files/initiate-upload", map[string]any{
		"file_name": "binary.bin", "content_type": "application/x-binary", "file_size": 4,
	}, http.StatusCreated)
	failed := f.request(t, http.MethodPost, path+"/files/complete-upload", map[string]any{
		"file_id": unsupported["file_id"],
	}, http.StatusOK)
	if failed["processing_status"] != "failed" || failed["processing_error"] == nil {
		t.Fatalf("unsupported file was marked usable: %#v", failed)
	}
	listed := f.request(t, http.MethodGet, path+"/files", nil, http.StatusOK)
	if len(listed["items"].([]any)) != 2 {
		t.Fatalf("file list did not survive refresh: %#v", listed)
	}
	seedProjectAdminUser(t, f.env, "draft-file-other", "password1")
	otherClient, otherToken := projectAdminLogin(t, f.env, "draft-file-other", "password1")
	req := mustRequest(f.env.ctx, t, http.MethodGet, f.env.base+path+"/files", nil)
	req.Header.Set("Authorization", "Bearer "+otherToken)
	resp := mustDo(t, otherClient, req)
	resp.Body.Close()
	assertStatus(t, resp, http.StatusNotFound)
}

func newProjectDraftFixture(t *testing.T) (*draftFixture, uuid.UUID, uuid.UUID, uuid.UUID) {
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
	session, conversation, err := env.agentSvc.StartGlobalChatSession(authz.WithActorID(env.ctx, owner.ID), agentID, owner.ID, "anchor", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.agentRepo.UpdateConversationStatus(env.ctx, conversation.ID, "finished"); err != nil {
		t.Fatal(err)
	}
	return &draftFixture{env: env, client: client, token: token, base: "/api/v1/project-drafts"}, owner.ID, session.ID, agentID
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
	f, owner, session, _ := newProjectDraftFixture(t)
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
	f, _, session, _ := newProjectDraftFixture(t)
	f.request(t, "POST", f.base, map[string]any{"name": " ", "chat_session_id": session}, 400)
	d := f.request(t, "POST", f.base, map[string]any{"name": "有效", "chat_session_id": session}, 201)
	path := f.base + "/" + d["id"].(string)
	f.request(t, "PATCH", path, map[string]any{"name": " ", "expected_version": 1}, 400)
	if got := f.request(t, "GET", path, nil, 200); got["version"] != float64(1) || got["name"] != "有效" {
		t.Fatal("invalid update modified draft")
	}
}

func TestPrivateProjectDraftServiceRechecksAuthorityAndSession(t *testing.T) {
	f, owner, session, _ := newProjectDraftFixture(t)
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

func TestPrivateProjectDraftMultiTurnGenerationImportIsBoundedAndIdempotent(t *testing.T) {
	f, owner, session, agentID := newProjectDraftFixture(t)
	d := f.request(t, "POST", f.base, map[string]any{"name": "初始草稿", "goal": "目标", "chat_session_id": session}, 201)
	path := f.base + "/" + d["id"].(string)
	started := f.request(t, "POST", path+"/generate", map[string]any{
		"expected_version": 1,
		"agent_id":         agentID,
		"message":          "整理成项目草稿",
	}, 200)
	generation := started["generation"].(map[string]any)
	conversationID := uuid.MustParse(generation["conversation_id"].(string))
	itemID := uuid.NewString()
	valid := `{"name":"生成项目","goal":"完成目标","items":[{"id":"` + itemID + `","title":"首项","requirements":"要求","criteria":"标准","dependencies":[]}],"participants":[],"materials":[]}`
	events := []*agentdom.AgentConversationEvent{
		{ID: uuid.New(), ConversationID: conversationID, EventIndex: 0, EventType: "user_message", EventSource: "user", Payload: map[string]any{"content": map[string]any{"type": "text", "text": "整理"}}},
		{ID: uuid.New(), ConversationID: conversationID, EventIndex: 1, EventType: "agent_thought_chunk", EventSource: "agent", Payload: map[string]any{"content": map[string]any{"type": "text", "text": "hidden"}}},
		{ID: uuid.New(), ConversationID: conversationID, EventIndex: 2, EventType: "agent_message_chunk", EventSource: "agent", Payload: map[string]any{"content": map[string]any{"type": "text", "text": valid}}},
		{ID: uuid.New(), ConversationID: conversationID, EventIndex: 3, EventType: "turn_end", EventSource: "system", Payload: map[string]any{"stopReason": "end_turn"}},
	}
	for _, event := range events {
		if err := f.env.agentRepo.CreateConversationEvent(f.env.ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.env.agentRepo.UpdateConversationStatus(f.env.ctx, conversationID, "finished"); err != nil {
		t.Fatal(err)
	}
	imported := f.request(t, "POST", path+"/import", map[string]any{
		"expected_version": 1,
		"generation_id":    generation["id"],
	}, 200)
	if imported["version"] != float64(2) || imported["name"] != "生成项目" {
		t.Fatalf("unexpected imported draft: %#v", imported)
	}
	retry := f.request(t, "POST", path+"/import", map[string]any{
		"expected_version": 1,
		"generation_id":    generation["id"],
	}, 200)
	if retry["version"] != float64(2) || retry["name"] != "生成项目" {
		t.Fatalf("import retry created a second result: %#v", retry)
	}
	if got := f.request(t, "GET", path, nil, 200); got["owner_user_id"] != owner.String() {
		t.Fatal("import changed draft owner")
	}
}

func TestPrivateProjectDraftGenerationFailureIsRecoverable(t *testing.T) {
	f, _, session, agentID := newProjectDraftFixture(t)
	d := f.request(t, "POST", f.base, map[string]any{"name": "可恢复", "chat_session_id": session}, 201)
	path := f.base + "/" + d["id"].(string)
	started := f.request(t, "POST", path+"/generate", map[string]any{
		"expected_version": 1,
		"agent_id":         agentID,
		"message":          "生成",
	}, 200)
	generation := started["generation"].(map[string]any)
	conversationID := uuid.MustParse(generation["conversation_id"].(string))
	for _, event := range []*agentdom.AgentConversationEvent{
		{ID: uuid.New(), ConversationID: conversationID, EventIndex: 0, EventType: "user_message", EventSource: "user", Payload: map[string]any{}},
		{ID: uuid.New(), ConversationID: conversationID, EventIndex: 1, EventType: "agent_message_chunk", EventSource: "agent", Payload: map[string]any{"content": map[string]any{"type": "text", "text": "not json"}}},
		{ID: uuid.New(), ConversationID: conversationID, EventIndex: 2, EventType: "turn_end", EventSource: "system", Payload: map[string]any{"stopReason": "end_turn"}},
	} {
		if err := f.env.agentRepo.CreateConversationEvent(f.env.ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.env.agentRepo.UpdateConversationStatus(f.env.ctx, conversationID, "finished"); err != nil {
		t.Fatal(err)
	}
	f.request(t, "POST", path+"/import", map[string]any{
		"expected_version": 1,
		"generation_id":    generation["id"],
	}, 400)
	got := f.request(t, "GET", path, nil, 200)
	if got["version"] != float64(1) || got["name"] != "可恢复" || got["generation"].(map[string]any)["status"] != "failed" {
		t.Fatalf("failed generation changed draft unexpectedly: %#v", got)
	}
}

func TestPrivateProjectDraftEditInvalidatesStaleGeneration(t *testing.T) {
	f, _, session, agentID := newProjectDraftFixture(t)
	d := f.request(t, "POST", f.base, map[string]any{"name": "旧草稿", "chat_session_id": session}, 201)
	path := f.base + "/" + d["id"].(string)
	started := f.request(t, "POST", path+"/generate", map[string]any{
		"expected_version": 1,
		"agent_id":         agentID,
		"message":          "生成",
	}, 200)
	f.request(t, "PATCH", path, map[string]any{"name": "人工修订", "expected_version": 1}, 200)
	got := f.request(t, "GET", path, nil, 200)
	if got["version"] != float64(2) || got["name"] != "人工修订" || got["generation"].(map[string]any)["status"] != "failed" {
		t.Fatalf("manual edit did not invalidate generation: %#v", got)
	}
	f.request(t, "POST", path+"/import", map[string]any{
		"expected_version": 1,
		"generation_id":    started["generation"].(map[string]any)["id"],
	}, 409)
}
