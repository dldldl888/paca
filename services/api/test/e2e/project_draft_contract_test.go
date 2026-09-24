package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	docdom "github.com/Paca-AI/api/internal/domain/doc"
	draftdom "github.com/Paca-AI/api/internal/domain/draft"
	projectdom "github.com/Paca-AI/api/internal/domain/project"
	"github.com/Paca-AI/api/internal/platform/authz"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	docsvc "github.com/Paca-AI/api/internal/service/doc"
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

func TestPrivateProjectDraftMaterialSearchPaginatesAccessibleProjects(t *testing.T) {
	f, owner, _, _ := newProjectDraftFixture(t)
	ownerCtx := authz.WithActorID(f.env.ctx, owner)
	for i := 0; i < 100; i++ {
		prefix := fmt.Sprintf("P%03d", i)
		if _, err := f.env.projectSvc.Create(ownerCtx, projectdom.CreateProjectInput{
			Name:         fmt.Sprintf("page-project-%03d", i),
			TaskIDPrefix: prefix,
			CreatedBy:    &owner,
		}); err != nil {
			t.Fatalf("create accessible project %d: %v", i, err)
		}
	}
	last, err := f.env.projectSvc.Create(ownerCtx, projectdom.CreateProjectInput{
		Name:         "page-project-late",
		TaskIDPrefix: "LATE",
		CreatedBy:    &owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.env.db.Exec(`INSERT INTO documents(id,project_id,title,content,visibility,owner_user_id) VALUES($1,$2,'late page material','[]','project',$3)`, uuid.New(), last.ID, owner); err != nil {
		t.Fatal(err)
	}

	docService := docsvc.New(pg.NewDocumentRepository(f.env.db), f.env.projectRepo)
	service := draftsvc.New(pg.NewDraftRepository(f.env.db), f.env.agentSvc, f.env.projectRepo).
		WithDocuments(docService).
		WithAuthorizer(authz.NewAuthorizer(pg.NewAuthzPermissionStore(f.env.db)))
	items, err := service.SearchProjectDraftMaterials(authz.WithHumanSession(ownerCtx), "late page material", 20)
	if err != nil || len(items) != 1 || items[0].Title != "late page material" {
		t.Fatalf("material search did not scan beyond the first project page: items=%#v err=%v", items, err)
	}
}

func TestPrivateProjectDraftWebSearchReportsUnavailableProvider(t *testing.T) {
	f, _, _, _ := newProjectDraftFixture(t)
	result := f.request(t, http.MethodGet, f.base+"/web-search?q=public", nil, http.StatusServiceUnavailable)
	if result["error_code"] != "SERVICE_UNAVAILABLE" {
		t.Fatalf("expected explicit provider failure, got %#v", result)
	}
}

func TestPrivateProjectDraftPreviewBindsVersionAndReportsIssues(t *testing.T) {
	f, owner, session, _ := newProjectDraftFixture(t)
	d := f.request(t, http.MethodPost, f.base, map[string]any{
		"name": "预览项目", "goal": "验证预览", "chat_session_id": session,
	}, http.StatusCreated)
	path := f.base + "/" + d["id"].(string)
	empty := f.request(t, http.MethodPost, path+"/preview", map[string]any{"expected_version": 1}, http.StatusOK)
	if empty["can_publish"] != false || len(empty["issues"].([]any)) == 0 {
		t.Fatalf("empty draft should be blocked by preview: %#v", empty)
	}
	itemID := uuid.NewString()
	updated := f.request(t, http.MethodPatch, path, map[string]any{
		"expected_version": 1,
		"name":             "预览项目", "goal": "验证预览", "items": []any{map[string]any{
			"id": itemID, "title": "可执行任务", "requirements": "交付要求", "criteria": "验收标准",
			"assignee_user_id": owner.String(), "due_date": "2026-12-01T18:00:00+08:00", "importance": 2,
			"dependencies": []any{},
		}},
		"participants": []any{map[string]any{"user_id": owner.String(), "role": "负责人"}},
		"materials":    []any{},
	}, http.StatusOK)
	if updated["version"] != float64(2) {
		t.Fatalf("expected updated draft version 2: %#v", updated)
	}
	valid := f.request(t, http.MethodPost, path+"/preview", map[string]any{"expected_version": 2}, http.StatusOK)
	if valid["version"] != float64(2) || valid["can_publish"] != true {
		t.Fatalf("valid draft should be publishable: %#v", valid)
	}
	f.request(t, http.MethodPost, path+"/preview", map[string]any{"expected_version": 1}, http.StatusConflict)
}

func TestPrivateProjectDraftPreviewProjectsMaterialsByAssignee(t *testing.T) {
	f, owner, session, _ := newProjectDraftFixture(t)
	seedTaskMemberUser(t, f.env, "draft-preview-peer", "password1")
	peer, err := f.env.userRepo.FindByUsername(f.env.ctx, "draft-preview-peer")
	if err != nil {
		t.Fatal(err)
	}
	firstItem, secondItem := uuid.NewString(), uuid.NewString()
	draft := f.request(t, http.MethodPost, f.base, map[string]any{
		"name": "员工范围预览", "goal": "验证任务资料投影", "chat_session_id": session,
		"items": []any{
			map[string]any{"id": firstItem, "title": "甲任务", "requirements": "甲要求", "criteria": "甲标准", "assignee_user_id": owner.String(), "due_date": "2026-12-01T18:00:00+08:00", "dependencies": []any{}},
			map[string]any{"id": secondItem, "title": "乙任务", "requirements": "乙要求", "criteria": "乙标准", "assignee_user_id": peer.ID.String(), "due_date": "2026-12-02T18:00:00+08:00", "dependencies": []any{}},
		},
		"participants": []any{
			map[string]any{"user_id": owner.String(), "role": "负责人"},
			map[string]any{"user_id": peer.ID.String(), "role": "执行人"},
		},
		"materials": []any{
			map[string]any{"id": uuid.NewString(), "kind": "document", "processed_status": "ready", "task_ids": []any{firstItem}},
			map[string]any{"id": uuid.NewString(), "kind": "document", "processed_status": "ready", "task_ids": []any{secondItem}},
			map[string]any{"id": uuid.NewString(), "kind": "document", "processed_status": "ready"},
		},
	}, http.StatusCreated)
	preview := f.request(t, http.MethodPost, f.base+"/"+draft["id"].(string)+"/preview", map[string]any{"expected_version": 1}, http.StatusOK)
	views, ok := preview["employee_views"].([]any)
	if !ok || len(views) != 2 {
		t.Fatalf("preview did not expose two employee projections: %#v", preview)
	}
	byUser := map[string]map[string]any{}
	for _, raw := range views {
		view, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("invalid employee projection: %#v", raw)
		}
		byUser[view["user_id"].(string)] = view
	}
	for _, userID := range []string{owner.String(), peer.ID.String()} {
		view, ok := byUser[userID]
		if !ok || len(view["items"].([]any)) != 1 || len(view["materials"].([]any)) != 2 {
			t.Fatalf("employee projection has incorrect scope for %s: %#v", userID, view)
		}
	}
	ownerItem := byUser[owner.String()]["items"].([]any)[0].(map[string]any)["id"]
	peerItem := byUser[peer.ID.String()]["items"].([]any)[0].(map[string]any)["id"]
	if ownerItem == peerItem {
		t.Fatalf("task item crossed employee projections: %#v", views)
	}
	ownerMaterials := byUser[owner.String()]["materials"].([]any)
	peerMaterials := byUser[peer.ID.String()]["materials"].([]any)
	for _, raw := range ownerMaterials {
		material := raw.(map[string]any)
		if ids, ok := material["task_ids"].([]any); ok && len(ids) > 0 && ids[0] == secondItem {
			t.Fatalf("owner projection included peer-only material: %#v", ownerMaterials)
		}
	}
	for _, raw := range peerMaterials {
		material := raw.(map[string]any)
		if ids, ok := material["task_ids"].([]any); ok && len(ids) > 0 && ids[0] == firstItem {
			t.Fatalf("peer projection included owner-only material: %#v", peerMaterials)
		}
	}
}

func TestPrivateProjectDraftPreviewRechecksInternalMaterialAccess(t *testing.T) {
	f, owner, session, _ := newProjectDraftFixture(t)
	projectID := createProjectForTasksViaAPI(t, f.env, f.client, f.token)
	seedUser(t, f.env, "draft-material-owner", "password1", "Material owner")
	materialUser, err := f.env.userRepo.FindByUsername(f.env.ctx, "draft-material-owner")
	if err != nil {
		t.Fatal(err)
	}
	docPath := f.env.base + "/api/v1/projects/" + projectID + "/docs"
	docID := uuid.New()
	if _, err := f.env.db.Exec(`INSERT INTO documents(id,project_id,title,content,visibility,owner_user_id) VALUES($1,$2,'可撤权资料','[]','project',$3)`, docID, projectID, materialUser.ID); err != nil {
		t.Fatal(err)
	}

	itemID := uuid.NewString()
	draft := f.request(t, http.MethodPost, f.base, map[string]any{
		"name":            "资料撤权预览",
		"goal":            "撤权后不能继续发布",
		"chat_session_id": session,
		"items": []any{map[string]any{
			"id": itemID, "title": "引用资料", "requirements": "核验", "criteria": "通过",
			"assignee_user_id": owner.String(), "due_date": "2026-12-01T18:00:00+08:00", "importance": 2,
			"dependencies": []any{},
		}},
		"participants": []any{map[string]any{"user_id": owner.String(), "role": "负责人"}},
		"materials": []any{map[string]any{
			"id": docID, "kind": "document", "source_type": "internal", "processed_status": "ready",
			"task_ids": []any{itemID}, "required": true,
		}},
	}, http.StatusCreated)
	previewPath := f.base + "/" + draft["id"].(string) + "/preview"
	allowed := f.request(t, http.MethodPost, previewPath, map[string]any{"expected_version": 1}, http.StatusOK)
	if allowed["can_publish"] != false {
		t.Fatalf("material without a confirmed sharing policy must block publication: %#v", allowed)
	}
	previewIssues, previewIssuesOK := allowed["issues"].([]any)
	if !previewIssuesOK || len(previewIssues) == 0 {
		t.Fatalf("blocked material preview did not explain the publication boundary: %#v", allowed)
	}
	policyIssue := false
	for _, raw := range previewIssues {
		issue, ok := raw.(map[string]any)
		if ok && issue["field"] == "materials" && strings.Contains(issue["message"].(string), "共享") {
			policyIssue = true
		}
	}
	if !policyIssue {
		t.Fatalf("preview did not report the unconfirmed material policy: %#v", previewIssues)
	}
	beforePublish := projectBusinessCounts(t, f.env)
	f.request(t, http.MethodPost, f.base+"/"+draft["id"].(string)+"/publish", map[string]any{
		"expected_version": 1,
		"publication_id":   uuid.New(),
	}, http.StatusBadRequest)
	if afterPublish := projectBusinessCounts(t, f.env); !reflect.DeepEqual(beforePublish, afterPublish) {
		t.Fatalf("blocked material publish left formal objects: %v -> %v", beforePublish, afterPublish)
	}

	accessReq := mustRequest(f.env.ctx, t, http.MethodPatch, docPath+"/"+docID.String()+"/access", jsonBody(t, map[string]any{
		"expected_access_version": 1,
		"visibility":              "private",
	}))
	accessReq.Header.Set("Authorization", "Bearer "+f.token)
	accessResp := mustDo(t, f.client, accessReq)
	defer accessResp.Body.Close()
	assertStatus(t, accessResp, http.StatusOK)

	blocked := f.request(t, http.MethodPost, previewPath, map[string]any{"expected_version": 1}, http.StatusOK)
	if blocked["can_publish"] != false {
		t.Fatalf("preview reused revoked internal material: %#v", blocked)
	}
	issues, ok := blocked["issues"].([]any)
	if !ok || len(issues) == 0 {
		t.Fatalf("preview did not explain revoked material: %#v", blocked)
	}
	revokedIssue := false
	for _, raw := range issues {
		issue, ok := raw.(map[string]any)
		if ok && issue["field"] == "materials[0]" && strings.Contains(issue["message"].(string), "不可读") {
			revokedIssue = true
		}
	}
	if !revokedIssue {
		t.Fatalf("preview did not report the revoked source material: %#v", issues)
	}
}

func TestPrivateProjectDraftUpdateRejectsRevokedInternalMaterial(t *testing.T) {
	f, owner, session, _ := newProjectDraftFixture(t)
	projectID := createProjectForTasksViaAPI(t, f.env, f.client, f.token)
	seedUser(t, f.env, "draft-update-material-owner", "password1", "Material owner")
	materialUser, err := f.env.userRepo.FindByUsername(f.env.ctx, "draft-update-material-owner")
	if err != nil {
		t.Fatal(err)
	}
	docID := uuid.New()
	if _, err := f.env.db.Exec(`INSERT INTO documents(id,project_id,title,content,visibility,owner_user_id) VALUES($1,$2,'更新撤权资料','[]','project',$3)`, docID, projectID, materialUser.ID); err != nil {
		t.Fatal(err)
	}
	itemID := uuid.NewString()
	content := map[string]any{
		"name": "资料写入撤权", "goal": "撤权后不能写回旧资料", "chat_session_id": session,
		"items": []any{map[string]any{
			"id": itemID, "title": "引用资料", "requirements": "核验", "criteria": "通过",
			"assignee_user_id": owner.String(), "due_date": "2026-12-01T18:00:00+08:00", "importance": 2,
			"dependencies": []any{},
		}},
		"participants": []any{map[string]any{"user_id": owner.String(), "role": "负责人"}},
		"materials": []any{map[string]any{
			"id": docID, "kind": "document", "source_type": "internal", "processed_status": "ready",
			"task_ids": []any{itemID}, "required": true,
		}},
	}
	draft := f.request(t, http.MethodPost, f.base, content, http.StatusCreated)
	path := f.base + "/" + draft["id"].(string)
	delete(content, "chat_session_id")
	content["expected_version"] = 1
	readable := f.request(t, http.MethodPatch, path, content, http.StatusOK)
	if readable["version"] != float64(2) || len(readable["materials"].([]any)) != 1 {
		t.Fatalf("readable material was not saved: %#v", readable)
	}
	accessReq := mustRequest(f.env.ctx, t, http.MethodPatch, f.env.base+"/api/v1/projects/"+projectID+"/docs/"+docID.String()+"/access", jsonBody(t, map[string]any{
		"expected_access_version": 1,
		"visibility":              "private",
	}))
	accessReq.Header.Set("Authorization", "Bearer "+f.token)
	accessResp := mustDo(t, f.client, accessReq)
	defer accessResp.Body.Close()
	assertStatus(t, accessResp, http.StatusOK)

	content["expected_version"] = 2
	content["name"] = "不能写入的修改"
	f.request(t, http.MethodPatch, path, content, http.StatusBadRequest)
	current := f.request(t, http.MethodGet, path, nil, http.StatusOK)
	if current["version"] != float64(2) || current["name"] != "资料写入撤权" || len(current["materials"].([]any)) != 1 {
		t.Fatalf("revoked material update changed the draft: %#v", current)
	}
	content["materials"] = []any{}
	removed := f.request(t, http.MethodPatch, path, content, http.StatusOK)
	if removed["version"] != float64(3) || len(removed["materials"].([]any)) != 0 {
		t.Fatalf("owner could not remove revoked material: %#v", removed)
	}
}

func TestPrivateProjectDraftPublishIsAtomicAndIdempotent(t *testing.T) {
	f, owner, session, _ := newProjectDraftFixture(t)
	seedTaskMemberUser(t, f.env, "draft-publish-peer", "password1")
	peer, err := f.env.userRepo.FindByUsername(f.env.ctx, "draft-publish-peer")
	if err != nil {
		t.Fatal(err)
	}
	itemID := uuid.NewString()
	d := f.request(t, http.MethodPost, f.base, map[string]any{
		"name": "正式发布项目", "goal": "验证发布事务", "chat_session_id": session,
		"items": []any{map[string]any{
			"id": itemID, "title": "执行任务", "requirements": "完成交付", "criteria": "通过验收",
			"assignee_user_id": peer.ID.String(), "due_date": "2026-12-01T18:00:00+08:00", "importance": 2,
			"dependencies": []any{},
		}},
		"participants": []any{
			map[string]any{"user_id": owner.String(), "role": "负责人"},
			map[string]any{"user_id": peer.ID.String(), "role": "执行人"},
		},
		"materials": []any{},
	}, http.StatusCreated)
	before := projectBusinessCounts(t, f.env)
	publication := uuid.NewString()
	path := f.base + "/" + d["id"].(string) + "/publish"
	first := f.request(t, http.MethodPost, path, map[string]any{
		"expected_version": 1, "publication_id": publication,
	}, http.StatusOK)
	if first["project_id"] == nil || len(first["task_ids"].(map[string]any)) != 1 {
		t.Fatalf("publish result missing stable ids: %#v", first)
	}
	after := projectBusinessCounts(t, f.env)
	if after[0] != before[0]+1 || after[2] != before[2]+1 || after[5] != before[5]+1 {
		t.Fatalf("publish did not create one project/task/notification: before=%v after=%v", before, after)
	}
	peerClient, peerToken := taskMemberLogin(t, f.env, "draft-publish-peer", "password1")
	notificationReq := mustRequest(f.env.ctx, t, http.MethodGet, f.env.base+"/api/v1/users/me/notifications?page_size=20", nil)
	notificationReq.Header.Set("Authorization", "Bearer "+peerToken)
	notificationResp := mustDo(t, peerClient, notificationReq)
	defer notificationResp.Body.Close()
	assertStatus(t, notificationResp, http.StatusOK)
	var notificationEnvelope envelope
	decodeJSON(t, notificationResp, &notificationEnvelope)
	notificationData := assertDataMap(t, notificationEnvelope)
	items, ok := notificationData["items"].([]any)
	if !ok || len(items) == 0 || items[0].(map[string]any)["task_id"] == nil {
		t.Fatalf("assigned employee did not recover a task notification: %#v", notificationData)
	}
	taskID := first["task_ids"].(map[string]any)[itemID].(string)
	taskReq := mustRequest(f.env.ctx, t, http.MethodGet, f.env.base+"/api/v1/projects/"+first["project_id"].(string)+"/tasks/"+taskID, nil)
	taskReq.Header.Set("Authorization", "Bearer "+peerToken)
	taskResp := mustDo(t, peerClient, taskReq)
	defer taskResp.Body.Close()
	assertStatus(t, taskResp, http.StatusOK)
	second := f.request(t, http.MethodPost, path, map[string]any{
		"expected_version": 1, "publication_id": publication,
	}, http.StatusOK)
	if second["project_id"] != first["project_id"] || len(second["task_ids"].(map[string]any)) != 1 {
		t.Fatalf("replay created a different publication: first=%#v second=%#v", first, second)
	}
	if got := projectBusinessCounts(t, f.env); !reflect.DeepEqual(got, after) {
		t.Fatalf("replay changed business counts: after=%v replay=%v", after, got)
	}
	f.request(t, http.MethodPost, path, map[string]any{
		"expected_version": 1, "publication_id": uuid.NewString(),
	}, http.StatusConflict)
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

func TestPrivateProjectDraftRejectsDependencyCycle(t *testing.T) {
	f, owner, session, _ := newProjectDraftFixture(t)
	firstID := uuid.NewString()
	secondID := uuid.NewString()
	f.request(t, http.MethodPost, f.base, map[string]any{
		"name": "循环依赖", "goal": "验证依赖图", "chat_session_id": session,
		"items": []any{
			map[string]any{
				"id": firstID, "title": "第一项", "requirements": "要求", "criteria": "标准",
				"assignee_user_id": owner.String(), "dependencies": []string{secondID},
			},
			map[string]any{
				"id": secondID, "title": "第二项", "requirements": "要求", "criteria": "标准",
				"assignee_user_id": owner.String(), "dependencies": []string{firstID},
			},
		},
		"participants": []any{map[string]any{"user_id": owner.String(), "role": "负责人"}},
		"materials":    []any{},
	}, http.StatusBadRequest)
}

func TestPrivateProjectDraftRejectsUnselectedAssignee(t *testing.T) {
	f, owner, session, _ := newProjectDraftFixture(t)
	f.request(t, http.MethodPost, f.base, map[string]any{
		"name": "未选负责人", "goal": "验证任务归属", "chat_session_id": session,
		"items": []any{map[string]any{
			"id": uuid.NewString(), "title": "任务", "requirements": "要求", "criteria": "标准",
			"assignee_user_id": uuid.NewString(), "dependencies": []string{},
		}},
		"participants": []any{map[string]any{"user_id": owner.String(), "role": "负责人"}},
		"materials":    []any{},
	}, http.StatusBadRequest)
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

func TestPrivateProjectDraftImportRejectsRevokedInternalMaterial(t *testing.T) {
	f, owner, session, agentID := newProjectDraftFixture(t)
	projectID := createProjectForTasksViaAPI(t, f.env, f.client, f.token)
	seedUser(t, f.env, "draft-import-material-owner", "password1", "Material owner")
	materialUser, err := f.env.userRepo.FindByUsername(f.env.ctx, "draft-import-material-owner")
	if err != nil {
		t.Fatal(err)
	}
	docID := uuid.New()
	if _, err := f.env.db.Exec(`INSERT INTO documents(id,project_id,title,content,visibility,owner_user_id) VALUES($1,$2,'导入撤权资料','[]','project',$3)`, docID, projectID, materialUser.ID); err != nil {
		t.Fatal(err)
	}
	d := f.request(t, http.MethodPost, f.base, map[string]any{
		"name": "导入撤权", "goal": "撤权后不能导入资料", "chat_session_id": session,
	}, http.StatusCreated)
	path := f.base + "/" + d["id"].(string)
	started := f.request(t, http.MethodPost, path+"/generate", map[string]any{
		"expected_version": 1, "agent_id": agentID, "message": "整理成项目草稿",
	}, http.StatusOK)
	generation := started["generation"].(map[string]any)
	conversationID := uuid.MustParse(generation["conversation_id"].(string))
	itemID := uuid.NewString()
	valid := fmt.Sprintf(`{"name":"导入项目","goal":"完成目标","items":[{"id":"%s","title":"资料任务","requirements":"要求","criteria":"标准","assignee_user_id":"%s","dependencies":[]}],"participants":[{"user_id":"%s","role":"负责人"}],"materials":[{"id":"%s","kind":"document","source_type":"internal","processed_status":"ready","task_ids":["%s"],"required":true}]}`,
		itemID, owner, owner, docID, itemID)
	events := []*agentdom.AgentConversationEvent{
		{ID: uuid.New(), ConversationID: conversationID, EventIndex: 0, EventType: "user_message", EventSource: "user", Payload: map[string]any{"content": map[string]any{"type": "text", "text": "整理"}}},
		{ID: uuid.New(), ConversationID: conversationID, EventIndex: 1, EventType: "agent_message_chunk", EventSource: "agent", Payload: map[string]any{"content": map[string]any{"type": "text", "text": valid}}},
		{ID: uuid.New(), ConversationID: conversationID, EventIndex: 2, EventType: "turn_end", EventSource: "system", Payload: map[string]any{"stopReason": "end_turn"}},
	}
	for _, event := range events {
		if err := f.env.agentRepo.CreateConversationEvent(f.env.ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.env.agentRepo.UpdateConversationStatus(f.env.ctx, conversationID, "finished"); err != nil {
		t.Fatal(err)
	}
	accessReq := mustRequest(f.env.ctx, t, http.MethodPatch, f.env.base+"/api/v1/projects/"+projectID+"/docs/"+docID.String()+"/access", jsonBody(t, map[string]any{
		"expected_access_version": 1,
		"visibility":              "private",
	}))
	accessReq.Header.Set("Authorization", "Bearer "+f.token)
	accessResp := mustDo(t, f.client, accessReq)
	defer accessResp.Body.Close()
	assertStatus(t, accessResp, http.StatusOK)

	f.request(t, http.MethodPost, path+"/import", map[string]any{
		"expected_version": 1,
		"generation_id":    generation["id"],
	}, http.StatusBadRequest)
	current := f.request(t, http.MethodGet, path, nil, http.StatusOK)
	if current["version"] != float64(1) || current["name"] != "导入撤权" || current["generation"].(map[string]any)["status"] != "failed" {
		t.Fatalf("revoked material import changed the draft: %#v", current)
	}
}

type transientDraftDocumentService struct{ err error }

func (f transientDraftDocumentService) CheckDocumentAccess(context.Context, uuid.UUID, uuid.UUID, docdom.AccessAction) (*docdom.Document, error) {
	return nil, f.err
}
func (transientDraftDocumentService) ListDocuments(context.Context, uuid.UUID, *uuid.UUID, *string, *string, *int) ([]*docdom.Document, bool, error) {
	return nil, false, nil
}
func (f transientDraftDocumentService) GetDocument(context.Context, uuid.UUID) (*docdom.Document, error) {
	return nil, f.err
}
func (f transientDraftDocumentService) CreateDocument(context.Context, docdom.CreateDocumentInput) (*docdom.Document, error) {
	return nil, f.err
}
func (f transientDraftDocumentService) UpdateDocument(context.Context, uuid.UUID, uuid.UUID, docdom.UpdateDocumentInput) (*docdom.Document, error) {
	return nil, f.err
}
func (f transientDraftDocumentService) DeleteDocument(context.Context, uuid.UUID, uuid.UUID) error {
	return f.err
}

func TestPrivateProjectDraftImportKeepsGenerationRunningOnTransientMaterialError(t *testing.T) {
	f, owner, session, agentID := newProjectDraftFixture(t)
	d := f.request(t, http.MethodPost, f.base, map[string]any{
		"name": "导入重试", "goal": "权限服务短暂不可用", "chat_session_id": session,
	}, http.StatusCreated)
	path := f.base + "/" + d["id"].(string)
	started := f.request(t, http.MethodPost, path+"/generate", map[string]any{
		"expected_version": 1, "agent_id": agentID, "message": "整理成项目草稿",
	}, http.StatusOK)
	generation := started["generation"].(map[string]any)
	conversationID := uuid.MustParse(generation["conversation_id"].(string))
	itemID, documentID := uuid.NewString(), uuid.NewString()
	valid := fmt.Sprintf(`{"name":"可重试项目","goal":"完成目标","items":[{"id":"%s","title":"资料任务","requirements":"要求","criteria":"标准","assignee_user_id":"%s","dependencies":[]}],"participants":[{"user_id":"%s","role":"负责人"}],"materials":[{"id":"%s","kind":"document","source_type":"internal","processed_status":"ready","task_ids":["%s"],"required":true}]}`,
		itemID, owner, owner, documentID, itemID)
	events := []*agentdom.AgentConversationEvent{
		{ID: uuid.New(), ConversationID: conversationID, EventIndex: 0, EventType: "user_message", EventSource: "user", Payload: map[string]any{"content": map[string]any{"type": "text", "text": "整理"}}},
		{ID: uuid.New(), ConversationID: conversationID, EventIndex: 1, EventType: "agent_message_chunk", EventSource: "agent", Payload: map[string]any{"content": map[string]any{"type": "text", "text": valid}}},
		{ID: uuid.New(), ConversationID: conversationID, EventIndex: 2, EventType: "turn_end", EventSource: "system", Payload: map[string]any{"stopReason": "end_turn"}},
	}
	for _, event := range events {
		if err := f.env.agentRepo.CreateConversationEvent(f.env.ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.env.agentRepo.UpdateConversationStatus(f.env.ctx, conversationID, "finished"); err != nil {
		t.Fatal(err)
	}
	transient := errors.New("temporary document authorization outage")
	authorizer := authz.NewAuthorizer(pg.NewAuthzPermissionStore(f.env.db)).WithAgentRoleResolver(pg.NewAuthzPermissionStore(f.env.db))
	service := draftsvc.New(pg.NewDraftRepository(f.env.db), f.env.agentSvc, f.env.projectRepo).
		WithAuthorizer(authorizer).
		WithDocuments(transientDraftDocumentService{err: transient})
	ctx := authz.WithHumanSession(authz.WithActorID(f.env.ctx, owner))
	if _, err := service.ImportProjectDraft(ctx, uuid.MustParse(d["id"].(string)), 1, uuid.MustParse(generation["id"].(string))); !errors.Is(err, transient) {
		t.Fatalf("transient material error was not returned for retry: %v", err)
	}
	current := f.request(t, http.MethodGet, path, nil, http.StatusOK)
	if current["version"] != float64(1) || current["generation"].(map[string]any)["status"] != "running" {
		t.Fatalf("transient material error made generation terminal: %#v", current)
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
