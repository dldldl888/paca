package e2e_test

import (
	"context"
	"encoding/json"
	attachmentdom "github.com/Paca-AI/api/internal/domain/attachment"
	docdom "github.com/Paca-AI/api/internal/domain/doc"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/database"
	pgRepo "github.com/Paca-AI/api/internal/repository/postgres"
	docsvc "github.com/Paca-AI/api/internal/service/doc"
	"github.com/Paca-AI/api/migrations"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDocumentScopeDefaultPrivate(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "doc-owner", "password1")
	ownerClient, ownerToken := taskMemberLogin(t, env, "doc-owner", "password1")
	pid := createProjectForTasksViaAPI(t, env, ownerClient, ownerToken)
	seedUser(t, env, "doc-other", "password1", "Other")
	other, err := env.userRepo.FindByUsername(env.ctx, "doc-other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, uuid.New(), pid, other.ID); err != nil {
		t.Fatal(err)
	}
	otherClient, otherToken := taskMemberLogin(t, env, "doc-other", "password1")
	base := env.base + "/api/v1/projects/" + pid + "/docs"
	req := mustRequest(env.ctx, t, "POST", base, jsonBody(t, map[string]any{"title": "owner private", "content": []any{}}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	resp := mustDo(t, ownerClient, req)
	assertStatus(t, resp, http.StatusCreated)
	var result envelope
	decodeJSON(t, resp, &result)
	resp.Body.Close()
	data := assertDataMap(t, result)
	id := data["id"].(string)
	for _, actor := range []struct {
		client *http.Client
		token  string
		want   int
	}{{ownerClient, ownerToken, 200}, {otherClient, otherToken, 404}} {
		req = mustRequest(env.ctx, t, "GET", base+"/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+actor.token)
		resp = mustDo(t, actor.client, req)
		resp.Body.Close()
		if resp.StatusCode != actor.want {
			t.Fatalf("default-private real HTTP: got %d want %d", resp.StatusCode, actor.want)
		}
	}
}

// HTTP matrix deliberately exercises the existing shared gate and real storage.
func TestDocumentScopeAndSourceHTTP(t *testing.T) {
	env := newE2EEnv(t)
	startFileDelivery(t, env)
	seedTaskMemberUser(t, env, "scope-manager", "password1")
	managerClient, managerToken := taskMemberLogin(t, env, "scope-manager", "password1")
	pid := createProjectForTasksViaAPI(t, env, managerClient, managerToken)
	type actor struct {
		client       *http.Client
		token        string
		user, member uuid.UUID
	}
	manager, _ := env.userRepo.FindByUsername(env.ctx, "scope-manager")
	actors := []actor{{client: managerClient, token: managerToken, user: manager.ID}}
	for _, name := range []string{"scope-a", "scope-b"} {
		seedUser(t, env, name, "password1", name)
		u, e := env.userRepo.FindByUsername(env.ctx, name)
		if e != nil {
			t.Fatal(e)
		}
		member := uuid.New()
		if _, e = env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, member, pid, u.ID); e != nil {
			t.Fatal(e)
		}
		c, token := taskMemberLogin(t, env, name, "password1")
		actors = append(actors, actor{c, token, u.ID, member})
	}
	seedUser(t, env, "scope-readall-only", "password1", "Read all only")
	readOnly, _ := env.userRepo.FindByUsername(env.ctx, "scope-readall-only")
	readRole := uuid.New()
	if _, e := env.db.Exec(`INSERT INTO project_roles(id,project_id,role_name,permissions)VALUES($1,$2,'Read all only','{"docs.read":true,"tasks.read":true,"tasks.read_all":true}')`, readRole, pid); e != nil {
		t.Fatal(e)
	}
	if _, e := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type)VALUES($1,$2,$3,$4,'human')`, uuid.New(), pid, readOnly.ID, readRole); e != nil {
		t.Fatal(e)
	}
	readClient, readToken := taskMemberLogin(t, env, "scope-readall-only", "password1")
	actors = append(actors, actor{client: readClient, token: readToken, user: readOnly.ID})
	request := func(index int, method, path string, body any, want int) map[string]any {
		t.Helper()
		// Allow participant processing after a successful authority change, but do not recover failures artificially.
		waitFileGate(t, env)
		req := mustRequest(env.ctx, t, method, env.base+path, nil)
		if body != nil {
			req = mustRequest(env.ctx, t, method, env.base+path, jsonBody(t, body))
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+actors[index].token)
		resp := mustDo(t, actors[index].client, req)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s actor=%d got=%d want=%d body=%s", method, path, index, resp.StatusCode, want, raw)
		}
		if want != 200 && want != 201 {
			if strings.Contains(string(raw), "source_project_id") || strings.Contains(string(raw), "canonical confidential") {
				t.Fatal("denied response leaked source metadata")
			}
			return nil
		}
		var result struct {
			Data map[string]any `json:"data"`
		}
		if e := json.Unmarshal(raw, &result); e != nil {
			t.Fatal(e)
		}
		return result.Data
	}
	base := "/api/v1/projects/" + pid + "/docs"
	create := func(index int, title string, extra map[string]any) string {
		body := map[string]any{"title": title}
		for k, v := range extra {
			body[k] = v
		}
		return request(index, "POST", base, body, 201)["id"].(string)
	}
	t.Run("private_all_surfaces_and_pagination", func(t *testing.T) {
		a := create(1, "B-secret-a", map[string]any{"content": []any{}})
		b := create(2, "C-visible-b", nil)
		c := create(2, "A-visible-b", nil)
		for _, suffix := range []string{"", "/snapshots", "/activities"} {
			request(2, "GET", base+"/"+a+suffix, nil, 404)
			request(1, "GET", base+"/"+a+suffix, nil, 200)
		}
		request(0, "GET", base+"/"+a, nil, 404)
		seedUser(t, env, "scope-star", "password1", "Explicit wildcard")
		assignGlobalRolesByName(t, env, "scope-star", "SUPER_ADMIN")
		starClient, starToken := taskMemberLogin(t, env, "scope-star", "password1")
		starReq := mustRequest(env.ctx, t, "GET", env.base+base+"/"+a, nil)
		starReq.Header.Set("Authorization", "Bearer "+starToken)
		starResp := mustDo(t, starClient, starReq)
		starResp.Body.Close()
		assertStatus(t, starResp, 200)
		t.Log("explicit persisted global * may read private; ordinary project manager remains denied")

		request(2, "PATCH", base+"/"+a, map[string]any{"title": "stolen"}, 404)
		request(2, "DELETE", base+"/"+a, nil, 404)
		request(2, "POST", base+"/"+a+"/comments", map[string]any{"content": map[string]any{"text": "leak"}}, 404)
		request(2, "POST", base+"/"+a+"/files/initiate-upload", map[string]any{"file_name": "x.txt", "content_type": "text/plain", "file_size": 1}, 404)
		request(1, "PATCH", base+"/"+a, map[string]any{"visibility": "project"}, 400)
		request(1, "POST", base, map[string]any{"title": "forged", "owner_user_id": actors[2].user}, 400)
		first := request(2, "GET", base+"?page_size=1", nil, 200)
		if len(first["items"].([]any)) != 1 || first["items"].([]any)[0].(map[string]any)["id"] != c {
			t.Fatalf("first page: %+v", first)
		}
		next := first["next_cursor"].(string)
		second := request(2, "GET", base+"?page_size=1&cursor="+url.QueryEscape(next), nil, 200)
		if len(second["items"].([]any)) != 1 || second["items"].([]any)[0].(map[string]any)["id"] != b || second["next_cursor"] != nil {
			t.Fatalf("second page: %+v", second)
		}
		search := request(2, "GET", base+"?search=secret", nil, 200)
		if len(search["items"].([]any)) != 0 {
			t.Fatal("search leaked")
		}
	})
	t.Run("task_assignment_version_and_empty_repair", func(t *testing.T) {
		taskBase := "/api/v1/projects/" + pid + "/tasks"
		task := request(0, "POST", taskBase, map[string]any{"title": "doc task", "assignee_ids": []uuid.UUID{actors[1].member}}, 201)["id"].(string)
		doc := create(0, "task scope", map[string]any{"visibility": "task", "task_ids": []string{task}})
		request(1, "GET", base+"/"+doc, nil, 200)
		request(2, "GET", base+"/"+doc, nil, 404)
		request(0, "PATCH", taskBase+"/"+task, map[string]any{"assignee_ids": []uuid.UUID{actors[2].member}}, 200)
		request(1, "GET", base+"/"+doc, nil, 404)
		request(2, "GET", base+"/"+doc, nil, 200)
		request(0, "PATCH", base+"/"+doc+"/access", map[string]any{"expected_access_version": 1, "visibility": "task", "task_ids": []string{}}, 400)
		request(0, "PATCH", base+"/"+doc+"/access", map[string]any{"expected_access_version": 1, "visibility": "project"}, 200)
		request(0, "PATCH", base+"/"+doc+"/access", map[string]any{"expected_access_version": 1, "visibility": "private"}, 409)
		request(0, "PATCH", base+"/"+doc+"/access", map[string]any{"expected_access_version": 2, "visibility": "task", "task_ids": []string{task}}, 200)
		request(3, "GET", base+"/"+doc, nil, 200)
		request(0, "DELETE", taskBase+"/"+task, nil, 200)
		request(2, "GET", base+"/"+doc, nil, 404)
		request(3, "GET", base+"/"+doc, nil, 404)
		empty := request(0, "GET", base+"/"+doc, nil, 200)
		if len(empty["task_ids"].([]any)) != 0 {
			t.Fatal("deleted tasks remained active associations")
		}
		discovered := request(0, "GET", base+"?search=task%20scope", nil, 200)["items"].([]any)
		if len(discovered) != 1 || discovered[0].(map[string]any)["id"] != doc {
			t.Fatalf("manager cannot discover empty task document %+v", discovered)
		}
		for _, index := range []int{1, 2, 3} {
			items := request(index, "GET", base+"?search=task%20scope", nil, 200)["items"].([]any)
			if len(items) != 0 {
				t.Fatal("empty task scope leaked to non-manager")
			}
		}
		request(0, "PATCH", base+"/"+doc+"/access", map[string]any{"expected_access_version": empty["access_version"], "visibility": "private"}, 200)
	})
	t.Run("source_intersection_history_and_real_file", func(t *testing.T) {
		source := create(0, "source secret", map[string]any{"visibility": "project", "content": []any{}})
		reference := create(0, "reference", map[string]any{"visibility": "project", "source_document_id": source})
		request(2, "GET", base+"/"+reference, nil, 200)
		request(0, "POST", base, map[string]any{"source_document_id": source, "content": []any{}}, 400)
		request(0, "POST", base, map[string]any{"source_document_id": reference}, 400)
		file := &attachmentdom.File{ID: uuid.New(), Bucket: "paca-attachments-e2e", FileName: "source.txt", ContentType: "text/plain", FileSize: 12, UploadStatus: attachmentdom.UploadStatusUploaded, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		file.StorageKey = "docs/" + source + "/" + file.ID.String() + "/source.txt"
		if e := env.attachmentRepo.CreateFile(env.ctx, file); e != nil {
			t.Fatal(e)
		}
		if e := env.fileStore.PutObject(env.ctx, file.Bucket, file.StorageKey, file.ContentType, []byte("source bytes")); e != nil {
			t.Fatal(e)
		}
		filePath := base + "/" + reference + "/files/" + file.ID.String()
		saved := request(2, "GET", filePath+"/download-url", nil, 200)
		t.Logf("reference URL: %+v", saved)
		if saved["url"] != filePath+"/content" {
			t.Fatal("reference URL lost target identity")
		}
		for _, doc := range []string{reference, source} {
			req := mustRequest(env.ctx, t, "GET", env.base+base+"/"+doc+"/files/"+file.ID.String()+"/content", nil)
			req.Header.Set("Authorization", "Bearer "+actors[2].token)
			resp := mustDo(t, actors[2].client, req)
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || string(raw) != "source bytes" {
				t.Fatalf("real source delivery %d %s", resp.StatusCode, raw)
			}
		}
		request(0, "PATCH", base+"/"+source+"/access", map[string]any{"expected_access_version": 1, "visibility": "private"}, 200)
		for _, doc := range []string{reference, source} {
			for _, suffix := range []string{"", "/snapshots", "/activities", "/files/" + file.ID.String() + "/download-url"} {
				request(2, "GET", base+"/"+doc+suffix, nil, 404)
			}
			for _, method := range []string{"GET", "HEAD"} {
				req := mustRequest(env.ctx, t, method, env.base+base+"/"+doc+"/files/"+file.ID.String()+"/content", nil)
				req.Header.Set("Authorization", "Bearer "+actors[2].token)
				req.Header.Set("Range", "bytes=0-3")
				resp := mustDo(t, actors[2].client, req)
				resp.Body.Close()
				assertStatus(t, resp, 404)
			}
		}
		request(0, "DELETE", base+"/"+source, nil, 204)
		request(0, "GET", base+"/"+reference, nil, 404)
	})
}

func TestDocumentCredentialAndAtomicScopeBoundary(t *testing.T) {
	env := newE2EEnv(t)
	coordinator := startFileDelivery(t, env)
	seedTaskMemberUser(t, env, "doc-credential", "password1")
	client, token := taskMemberLogin(t, env, "doc-credential", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	user, _ := env.userRepo.FindByUsername(env.ctx, "doc-credential")
	_, key := createAPIKey(t, env, client, token, "doc-personal-key")
	base := "/api/v1/projects/" + pid.String() + "/docs"
	request := func(credential, method, path string, body any, want int) map[string]any {
		t.Helper()
		waitFileGate(t, env)
		req := mustRequest(env.ctx, t, method, env.base+path, jsonBody(t, body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+credential)
		if credential == key {
			req.Header.Set("Authorization", "ApiKey "+credential)
		}
		resp := mustDo(t, &http.Client{}, req)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s status=%d want=%d %s", method, path, resp.StatusCode, want, raw)
		}
		if want != 200 && want != 201 {
			return nil
		}
		var result struct {
			Data map[string]any `json:"data"`
		}
		if e := json.Unmarshal(raw, &result); e != nil {
			t.Fatal(e)
		}
		return result.Data
	}
	request(key, "POST", base, map[string]any{"title": "API key owner"}, 403)
	doc := request(token, "POST", base, map[string]any{"title": "human owner"}, 201)["id"].(string)
	request(key, "PATCH", base+"/"+doc+"/access", map[string]any{"visibility": "project", "expected_access_version": 1}, 403)
	request(key, "PATCH", base+"/"+doc, map[string]any{"title": "existing edit remains allowed"}, 200)
	repo := pgRepo.NewDocumentRepository(env.db)
	actor := authz.WithActorID(env.ctx, user.ID)
	trusted := authz.WithHumanSession(actor)
	for _, ctx := range []context.Context{actor, authz.WithAgentCredential(trusted), authz.WithAgentID(trusted, uuid.New())} {
		d := &docdom.Document{ID: uuid.New(), ProjectID: pid, Title: "untrusted owner", CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if e := repo.CreateDocument(ctx, d); e == nil {
			t.Fatal("untrusted create succeeded")
		}
	}
	bounded, e := authz.WithTaskServiceGrant(trusted, authz.TaskServiceGrant{Name: "bounded", ProjectID: pid, TaskIDs: []uuid.UUID{uuid.New()}, Action: taskdom.AccessManage})
	if e != nil {
		t.Fatal(e)
	}
	if e = repo.CreateDocument(bounded, &docdom.Document{ID: uuid.New(), ProjectID: pid, Visibility: "project", Title: "bounded", CreatedAt: time.Now(), UpdatedAt: time.Now()}); e == nil {
		t.Fatal("bounded grant expanded audience")
	}
	request(token, "PATCH", base+"/"+doc+"/access", map[string]any{"visibility": "project", "expected_access_version": 1}, 200)
	before, e := coordinator.State(env.ctx)
	if e != nil {
		t.Fatal(e)
	}
	request(token, "PATCH", base+"/"+doc+"/access", map[string]any{"visibility": "private", "expected_access_version": 1}, 409)
	waitFileGate(t, env)
	current := request(token, "GET", base+"/"+doc, nil, 200)
	after, e := coordinator.State(env.ctx)
	if e != nil {
		t.Fatal(e)
	}
	if current["visibility"] != "project" || current["access_version"] != float64(2) || after.Blocked || before.Epoch == after.Epoch {
		t.Fatalf("stale mutation changed state or stranded gate: %+v %+v", current, after)
	}
	// Controlled DB fault only in the isolated fixture: fail after UPDATE while
	// inserting associations, then prove the transaction did not partially commit.
	task, e := env.taskSvc.CreateTask(actor, taskdom.CreateTaskInput{ProjectID: pid, Title: "atomic association"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = env.db.Exec(`CREATE FUNCTION reject_doc_link() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected association write failure'; END $$; CREATE TRIGGER fail_doc_link BEFORE INSERT ON document_tasks FOR EACH ROW EXECUTE FUNCTION reject_doc_link()`); e != nil {
		t.Fatal(e)
	}
	request(token, "PATCH", base+"/"+doc+"/access", map[string]any{"visibility": "task", "expected_access_version": 2, "task_ids": []uuid.UUID{task.ID}}, 500)
	var got struct {
		Visibility string `db:"visibility"`
		Version    int64  `db:"access_version"`
	}
	if e = env.db.Get(&got, `SELECT visibility,access_version FROM documents WHERE id=$1`, doc); e != nil {
		t.Fatal(e)
	}
	var links int
	if e = env.db.Get(&links, `SELECT count(*) FROM document_tasks WHERE document_id=$1`, doc); e != nil {
		t.Fatal(e)
	}
	if got.Visibility != "project" || got.Version != 2 || links != 0 {
		t.Fatalf("partial mutation: %+v links=%d", got, links)
	}
	failed, e := coordinator.State(env.ctx)
	if e != nil || !failed.Blocked {
		t.Fatalf("write fault must stay fail closed: %+v %v", failed, e)
	}
}

func TestDocumentScopeMigration(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "migration-doc-owner", "password1")
	client, token := taskMemberLogin(t, env, "migration-doc-owner", "password1")
	pid := createProjectForTasksViaAPI(t, env, client, token)
	user, _ := env.userRepo.FindByUsername(env.ctx, "migration-doc-owner")
	var member uuid.UUID
	if e := env.db.Get(&member, `SELECT id FROM project_members WHERE project_id=$1 AND user_id=$2`, pid, user.ID); e != nil {
		t.Fatal(e)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, e := env.db.Exec(query, args...); e != nil {
			t.Fatal(e)
		}
	}
	// Return the isolated schema to BASE, seed actual pre-migration rows, then
	// invoke the same migration runner used by production boot.
	exec(`DROP TABLE document_tasks; DROP FUNCTION check_document_task_project(); DROP TRIGGER document_source ON documents; DROP FUNCTION check_document_source(); ALTER TABLE documents DROP COLUMN visibility,DROP COLUMN owner_user_id,DROP COLUMN source_document_id,DROP COLUMN access_version; DELETE FROM schema_migrations WHERE filename='000060_document_access.sql'`)
	agent, agentMember := uuid.New(), uuid.New()
	exec(`INSERT INTO agents(id,project_id,name,handle,agent_type,agent_scope,acp_provider,llm_provider,llm_model,llm_api_key_secret) VALUES($1,$2,'Old Agent','old-doc-agent','acp','project','claude-code','','','')`, agent, pid)
	exec(`INSERT INTO project_members(id,project_id,agent_id,project_role_id,member_type) SELECT $1,$2,$3,id,'agent' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, agentMember, pid, agent)
	var ids []uuid.UUID
	for _, creator := range []*uuid.UUID{&member, &agentMember, nil} {
		id := uuid.New()
		ids = append(ids, id)
		exec(`INSERT INTO documents(id,project_id,title,created_by)VALUES($1,$2,'old document',$3)`, id, pid, creator)
	}
	if e := database.RunMigrationsFS(env.db.DB, migrations.FS); e != nil {
		t.Fatal(e)
	}
	for i, id := range ids {
		d, e := pgRepo.NewDocumentRepository(env.db).FindDocumentByID(env.ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		if d.Visibility != "legacy_project" || d.AccessVersion != 1 {
			t.Fatalf("bad legacy state: %+v", d)
		}
		if i == 0 && (d.OwnerUserID == nil || *d.OwnerUserID != user.ID || *d.OwnerUserID == member) {
			t.Fatal("created_by member was not resolved to human user")
		}
		if i != 0 && d.OwnerUserID != nil {
			t.Fatal("agent/null creator was assigned a guessed human owner")
		}
	}
	if _, e := env.db.Exec(`INSERT INTO documents(id,project_id,title) VALUES($1,$2,'no owner')`, uuid.New(), pid); e == nil {
		t.Fatal("default private allowed missing owner")
	}
	var defaultScope string
	if e := env.db.Get(&defaultScope, `SELECT column_default FROM information_schema.columns WHERE table_name='documents' AND column_name='visibility'`); e != nil || !strings.Contains(defaultScope, "private") {
		t.Fatal("new rows not private", defaultScope, e)
	}
}

func TestDocumentCrossProjectSourceAndHistoricalSurfaces(t *testing.T) {
	env := newE2EEnv(t)
	coordinator := startFileDelivery(t, env)
	seedTaskMemberUser(t, env, "cross-doc-manager", "password1")
	mc, mt := taskMemberLogin(t, env, "cross-doc-manager", "password1")
	target := createProjectForTasksViaAPI(t, env, mc, mt)
	source := createProjectForTasksViaAPI(t, env, mc, mt)
	seedUser(t, env, "cross-doc-reader", "password1", "Cross Reader")
	reader, _ := env.userRepo.FindByUsername(env.ctx, "cross-doc-reader")
	readerMember := uuid.New()
	if _, e := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, uuid.New(), target, reader.ID); e != nil {
		t.Fatal(e)
	}
	readRole := uuid.New()
	if _, e := env.db.Exec(`INSERT INTO project_roles(id,project_id,role_name,permissions)VALUES($1,$2,'Source read only','{"docs.read":true}');`, readRole, source); e != nil {
		t.Fatal(e)
	}
	if _, e := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type)VALUES($1,$2,$3,$4,'human')`, readerMember, source, reader.ID, readRole); e != nil {
		t.Fatal(e)
	}
	rc, rt := taskMemberLogin(t, env, "cross-doc-reader", "password1")
	req := func(client *http.Client, token, method, path string, body any, want int) map[string]any {
		t.Helper()
		waitFileGate(t, env)
		r := mustRequest(env.ctx, t, method, env.base+path, nil)
		if body != nil {
			r = mustRequest(env.ctx, t, method, env.base+path, jsonBody(t, body))
			r.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		resp := mustDo(t, client, r)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s status=%d want=%d %s", method, path, resp.StatusCode, want, raw)
		}
		if want != 200 && want != 201 {
			if strings.Contains(string(raw), "source_project_id") || strings.Contains(string(raw), "source_updated_at") || strings.Contains(string(raw), "canonical confidential") {
				t.Fatal("denied source projection leaked")
			}
			return nil
		}
		var result struct {
			Data map[string]any `json:"data"`
		}
		if e := json.Unmarshal(raw, &result); e != nil {
			t.Fatal(e)
		}
		return result.Data
	}
	sourceBase := "/api/v1/projects/" + source + "/docs"
	targetBase := "/api/v1/projects/" + target + "/docs"
	original := req(mc, mt, "POST", sourceBase, map[string]any{"title": "canonical confidential", "visibility": "project", "content": []any{}}, 201)["id"].(string)
	ref := req(mc, mt, "POST", targetBase, map[string]any{"title": "cross ref", "visibility": "project", "source_document_id": original}, 201)["id"].(string)
	private := req(rc, rt, "POST", targetBase, map[string]any{"title": "reader private"}, 201)["id"].(string)
	req(rc, rt, "PATCH", targetBase+"/"+ref, map[string]any{"title": "edited reference with read-only source"}, 200)
	projection := req(rc, rt, "GET", targetBase+"/"+ref, nil, 200)
	if projection["source_project_id"] != source || projection["source_updated_at"] == nil || projection["source_title"] != "canonical confidential" {
		t.Fatalf("missing authorized source projection: %+v", projection)
	}
	// Source content must never be copied into reference storage or snapshots.
	var copied int
	if e := env.db.Get(&copied, `SELECT (SELECT count(*) FROM documents WHERE id=$1 AND content IS NOT NULL)+(SELECT count(*) FROM doc_snapshots WHERE document_id=$1 AND content IS NOT NULL)`, ref); e != nil || copied != 0 {
		t.Fatal("reference copied source", copied, e)
	}
	req(mc, mt, "PATCH", sourceBase+"/"+original, map[string]any{"title": "canonical updated"}, 200)
	snaps := req(mc, mt, "GET", sourceBase+"/"+original+"/snapshots", nil, 200)["items"].([]any)
	if len(snaps) == 0 {
		t.Fatal("missing real snapshot")
	}
	snapshot := snaps[0].(map[string]any)["id"].(string)
	req(rc, rt, "GET", sourceBase+"/"+original+"/snapshots/"+snapshot, nil, 200)
	other := req(mc, mt, "POST", sourceBase, map[string]any{"title": "wrong snapshot URL", "visibility": "project"}, 201)["id"].(string)
	req(rc, rt, "GET", sourceBase+"/"+other+"/snapshots/"+snapshot, nil, 404)
	req(rc, rt, "POST", targetBase+"/"+private+"/comments", map[string]any{"content": map[string]any{"text": "private old comment"}}, 201)
	req(mc, mt, "GET", targetBase+"/"+private+"/activities", nil, 404)
	// All new scopes stay closed to anonymous users even in a public project.
	req(mc, mt, "PATCH", "/api/v1/projects/"+target, map[string]any{"is_public": true}, 200)
	for _, id := range []string{ref, private} {
		req(&http.Client{}, "", "GET", targetBase+"/"+id, nil, 404)
	}
	roleBefore, _ := coordinator.State(env.ctx)
	req(mc, mt, "PATCH", "/api/v1/projects/"+source+"/roles/"+readRole.String(), map[string]any{"role_name": "Source read only", "permissions": map[string]any{}}, 200)
	req(rc, rt, "GET", targetBase+"/"+ref, nil, 404)
	roleAfter, _ := coordinator.State(env.ctx)
	if roleBefore.Epoch == roleAfter.Epoch {
		t.Fatal("role revocation did not advance epoch")
	}
	req(mc, mt, "PATCH", "/api/v1/projects/"+source+"/roles/"+readRole.String(), map[string]any{"role_name": "Source read only", "permissions": map[string]any{"docs.read": true}}, 200)
	req(rc, rt, "GET", targetBase+"/"+ref, nil, 200)
	before, _ := coordinator.State(env.ctx)
	req(mc, mt, "DELETE", "/api/v1/projects/"+source+"/members/"+readerMember.String(), nil, 200)
	after, _ := coordinator.State(env.ctx)
	if before.Epoch == after.Epoch {
		t.Fatal("source member removal did not advance epoch")
	}
	for _, path := range []string{targetBase + "/" + ref, sourceBase + "/" + original, sourceBase + "/" + original + "/snapshots/" + snapshot} {
		// Source's project-entry middleware denies 403; the still-qualified target returns object-not-found.
		want := 403
		if strings.HasPrefix(path, targetBase) {
			want = 404
		}
		req(rc, rt, "GET", path, nil, want)
	}
	listing := req(rc, rt, "GET", targetBase+"?search=reference", nil, 200)["items"].([]any)
	if len(listing) != 0 {
		t.Fatal("source revocation leaked reference title")
	}
	// Even a private owner loses the project entry qualification immediately.
	var targetMember uuid.UUID
	if e := env.db.Get(&targetMember, `SELECT id FROM project_members WHERE project_id=$1 AND user_id=$2`, target, reader.ID); e != nil {
		t.Fatal(e)
	}
	req(mc, mt, "DELETE", "/api/v1/projects/"+target+"/members/"+targetMember.String(), nil, 200)
	req(rc, rt, "GET", targetBase+"/"+private, nil, 403)
}

func TestDocumentMultipleTasksAndConcurrentAccessVersion(t *testing.T) {
	env := newE2EEnv(t)
	startFileDelivery(t, env)
	seedTaskMemberUser(t, env, "multi-doc-manager", "password1")
	mc, mt := taskMemberLogin(t, env, "multi-doc-manager", "password1")
	pid := createProjectForTasksViaAPI(t, env, mc, mt)
	otherPid := createProjectForTasksViaAPI(t, env, mc, mt)
	manager, _ := env.userRepo.FindByUsername(env.ctx, "multi-doc-manager")
	ctx := authz.WithActorID(env.ctx, manager.ID)
	var members []uuid.UUID
	var clients []*http.Client
	var tokens []string
	for _, name := range []string{"multi-a", "multi-b"} {
		seedUser(t, env, name, "password1", name)
		user, _ := env.userRepo.FindByUsername(env.ctx, name)
		member := uuid.New()
		if _, e := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, member, pid, user.ID); e != nil {
			t.Fatal(e)
		}
		c, token := taskMemberLogin(t, env, name, "password1")
		members = append(members, member)
		clients = append(clients, c)
		tokens = append(tokens, token)
	}
	var tasks []uuid.UUID
	for _, member := range members {
		task, e := env.taskSvc.CreateTask(ctx, taskdom.CreateTaskInput{ProjectID: uuid.MustParse(pid), Title: "multiple association", AssigneeIDs: []uuid.UUID{member}})
		if e != nil {
			t.Fatal(e)
		}
		tasks = append(tasks, task.ID)
	}
	cross, e := env.taskSvc.CreateTask(ctx, taskdom.CreateTaskInput{ProjectID: uuid.MustParse(otherPid), Title: "cross task"})
	if e != nil {
		t.Fatal(e)
	}
	request := func(c *http.Client, token, method, path string, body any, want int) map[string]any {
		t.Helper()
		waitFileGate(t, env)
		r := mustRequest(env.ctx, t, method, env.base+path, nil)
		if body != nil {
			r = mustRequest(env.ctx, t, method, env.base+path, jsonBody(t, body))
			r.Header.Set("Content-Type", "application/json")
		}
		r.Header.Set("Authorization", "Bearer "+token)
		resp := mustDo(t, c, r)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s status=%d want=%d %s", method, path, resp.StatusCode, want, raw)
		}
		if want != 200 && want != 201 {
			return nil
		}
		var result struct {
			Data map[string]any `json:"data"`
		}
		if e := json.Unmarshal(raw, &result); e != nil {
			t.Fatal(e)
		}
		return result.Data
	}
	base := "/api/v1/projects/" + pid + "/docs"
	request(mc, mt, "POST", base, map[string]any{"title": "cross denied", "visibility": "task", "task_ids": []uuid.UUID{cross.ID}}, 404)
	id := request(mc, mt, "POST", base, map[string]any{"title": "multi doc", "visibility": "task", "task_ids": tasks}, 201)["id"].(string)
	for i, c := range clients {
		request(c, tokens[i], "GET", base+"/"+id, nil, 200)
	}
	// A docs.write editor without tasks.manage cannot share even their own private doc.
	own := request(clients[0], tokens[0], "POST", base, map[string]any{"title": "private"}, 201)["id"].(string)
	request(clients[0], tokens[0], "PATCH", base+"/"+own+"/access", map[string]any{"visibility": "project", "expected_access_version": 1}, 403)
	request(mc, mt, "PATCH", base+"/"+id+"/access", map[string]any{"visibility": "task", "task_ids": tasks[1:], "expected_access_version": 1}, 200)
	request(clients[0], tokens[0], "GET", base+"/"+id, nil, 404)
	request(clients[1], tokens[1], "GET", base+"/"+id, nil, 200)
	items := request(clients[1], tokens[1], "GET", base, nil, 200)["items"].([]any)
	if len(items) != 1 || len(items[0].(map[string]any)["task_ids"].([]any)) != 1 {
		t.Fatalf("list scope associations missing %+v", items)
	}
	statuses := make(chan int, 2)
	errs := make(chan error, 2)
	for _, visibility := range []string{"project", "private"} {
		body := jsonBody(t, map[string]any{"visibility": visibility, "expected_access_version": 2})
		r := mustRequest(env.ctx, t, "PATCH", env.base+base+"/"+id+"/access", body)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+mt)
		go func() {
			resp, e := mc.Do(r)
			if e != nil {
				errs <- e
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	got := map[int]int{}
	for range 2 {
		select {
		case e := <-errs:
			t.Fatal(e)
		case status := <-statuses:
			got[status]++
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent scope updates hung")
		}
	}
	if got[200] != 1 || got[409] != 1 {
		t.Fatalf("expected atomic version winner/loser: %+v", got)
	}
	waitFileGate(t, env)
	current := request(mc, mt, "GET", base+"/"+id, nil, 200)
	if current["access_version"] != float64(3) {
		t.Fatalf("bad concurrent version %+v", current)
	}
}

func TestDocumentAgentCredentialCannotBorrowBotAudience(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "doc-agent-owner", "password1")
	client, token := taskMemberLogin(t, env, "doc-agent-owner", "password1")
	pid := createProjectForTasksViaAPI(t, env, client, token)
	seedUser(t, env, "doc-agent-bot", "password1", "Bot")
	assignGlobalRolesByName(t, env, "doc-agent-bot", "SUPER_ADMIN")
	bot, _ := env.userRepo.FindByUsername(env.ctx, "doc-agent-bot")
	syntheticKey := "paca_doc_test_" + uuid.NewString()
	env.apiKeySvc.WithAgentKey(syntheticKey, bot.ID).WithAgentIdentityStore(env.agentRepo)
	owner, _ := env.userRepo.FindByUsername(env.ctx, "doc-agent-owner")
	ids := map[string]uuid.UUID{}
	for _, scope := range []string{"legacy_project", "project", "private"} {
		id := uuid.New()
		ids[scope] = id
		if _, e := env.db.Exec(`INSERT INTO documents(id,project_id,title,visibility,owner_user_id)VALUES($1,$2,'credential scope',$3,$4)`, id, pid, scope, owner.ID); e != nil {
			t.Fatal(e)
		}
	}
	for _, scope := range []string{"legacy_project", "project", "private"} {
		req := mustRequest(env.ctx, t, "GET", env.base+"/api/v1/projects/"+pid+"/docs/"+ids[scope].String(), nil)
		req.Header.Set("Authorization", "ApiKey "+syntheticKey)
		resp := mustDo(t, &http.Client{}, req)
		resp.Body.Close()
		want := 404
		if scope == "legacy_project" {
			want = 200
		}
		assertStatus(t, resp, want)
	}
	req := mustRequest(env.ctx, t, "POST", env.base+"/api/v1/projects/"+pid+"/docs", jsonBody(t, map[string]any{"title": "no guessed bot owner"}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "ApiKey "+syntheticKey)
	resp := mustDo(t, &http.Client{}, req)
	resp.Body.Close()
	assertStatus(t, resp, 403)
	agent := uuid.New()
	if _, e := env.db.Exec(`INSERT INTO agents(id,project_id,name,handle,agent_type,agent_scope,acp_provider,llm_provider,llm_model,llm_api_key_secret) VALUES($1,$2,'Verified agent','verified-doc-agent','acp','project','claude-code','','','')`, agent, pid); e != nil {
		t.Fatal(e)
	}
	if _, e := env.db.Exec(`INSERT INTO project_members(id,project_id,agent_id,project_role_id,member_type) SELECT $1,$2,$3,id,'agent' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, uuid.New(), pid, agent); e != nil {
		t.Fatal(e)
	}
	for _, scope := range []string{"legacy_project", "project", "private"} {
		for _, method := range []string{"GET", "PATCH"} {
			req = mustRequest(env.ctx, t, method, env.base+"/api/v1/projects/"+pid+"/docs/"+ids[scope].String(), nil)
			if method == "PATCH" {
				req = mustRequest(env.ctx, t, method, req.URL.String(), jsonBody(t, map[string]any{"title": "verified agent edit"}))
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("Authorization", "ApiKey "+syntheticKey)
			req.Header.Set("X-Agent-ID", agent.String())
			resp = mustDo(t, &http.Client{}, req)
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			want := 200
			if scope == "private" {
				want = 404
			}
			if resp.StatusCode != want {
				t.Fatalf("verified agent %s %s status=%d %s", scope, method, resp.StatusCode, raw)
			}
		}
	}
	task, e := env.taskSvc.CreateTask(authz.WithActorID(env.ctx, owner.ID), taskdom.CreateTaskInput{ProjectID: uuid.MustParse(pid), Title: "bounded agent doc task"})
	if e != nil {
		t.Fatal(e)
	}
	taskDoc := uuid.New()
	if _, e = env.db.Exec(`INSERT INTO documents(id,project_id,title,visibility,owner_user_id)VALUES($1,$2,'task grant doc','task',$3);`, taskDoc, pid, owner.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = env.db.Exec(`INSERT INTO document_tasks(document_id,task_id)VALUES($1,$2)`, taskDoc, task.ID); e != nil {
		t.Fatal(e)
	}
	agentCtx := authz.WithAgentID(authz.WithAgentCredential(authz.WithActorID(env.ctx, bot.ID)), agent)
	repo := pgRepo.NewDocumentRepository(env.db)
	if _, e = repo.CheckDocumentAccess(agentCtx, uuid.MustParse(pid), taskDoc, docdom.AccessRead); e == nil {
		t.Fatal("agent membership guessed task read grant")
	}
	bounded, e := authz.WithTaskServiceGrant(agentCtx, authz.TaskServiceGrant{Name: "verified-task-read", ProjectID: uuid.MustParse(pid), TaskIDs: []uuid.UUID{task.ID}, Action: taskdom.AccessRead})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = repo.CheckDocumentAccess(bounded, uuid.MustParse(pid), taskDoc, docdom.AccessRead); e != nil {
		t.Fatal("current bounded task grant denied", e)
	}
	req = mustRequest(env.ctx, t, "DELETE", env.base+"/api/v1/projects/"+pid+"/agents/"+agent.String(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp = mustDo(t, client, req)
	resp.Body.Close()
	assertStatus(t, resp, 200)
	if _, e = repo.CheckDocumentAccess(bounded, uuid.MustParse(pid), taskDoc, docdom.AccessRead); e == nil {
		t.Fatal("deleted agent retained a prior task grant")
	}

}

func TestDocumentTaskGrantCannotCreateOrChangeAudienceAcrossProjects(t *testing.T) {
	env := newE2EEnv(t)
	startFileDelivery(t, env)
	seedTaskMemberUser(t, env, "grant-doc-manager", "password1")
	client, token := taskMemberLogin(t, env, "grant-doc-manager", "password1")
	projects := []uuid.UUID{uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token)), uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))}
	user, err := env.userRepo.FindByUsername(env.ctx, "grant-doc-manager")
	if err != nil {
		t.Fatal(err)
	}
	trusted := authz.WithHumanSession(authz.WithActorID(env.ctx, user.ID))
	svc := docsvc.New(pgRepo.NewDocumentRepository(env.db), env.projectRepo)
	docs := map[uuid.UUID][]*docdom.Document{}
	var grantTask uuid.UUID
	for _, project := range projects {
		task, e := env.taskSvc.CreateTask(trusted, taskdom.CreateTaskInput{ProjectID: project, Title: "real grant task"})
		if e != nil {
			t.Fatal(e)
		}
		if project == projects[0] {
			grantTask = task.ID
		}
		for _, initial := range []string{"private", "task"} {
			in := docdom.CreateDocumentInput{ProjectID: project, Title: "unchanged audience", Visibility: initial}
			if initial == "task" {
				in.TaskIDs = []uuid.UUID{task.ID}
			}
			d, e := svc.CreateDocument(trusted, in)
			if e != nil {
				t.Fatal(e)
			}
			docs[project] = append(docs[project], d)
		}
	}
	bounded, err := authz.WithTaskServiceGrant(trusted, authz.TaskServiceGrant{Name: "document-boundary", ProjectID: projects[0], TaskIDs: []uuid.UUID{grantTask}, Action: taskdom.AccessManage})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func(t *testing.T) string {
		t.Helper()
		var state string
		if e := env.db.Get(&state, `SELECT jsonb_build_object('rows',(SELECT jsonb_agg(to_jsonb(d) ORDER BY d.id) FROM documents d),'links',(SELECT jsonb_agg(to_jsonb(dt) ORDER BY dt.document_id,dt.task_id) FROM document_tasks dt))::text`); e != nil {
			t.Fatal(e)
		}
		return state
	}
	for i, project := range projects {
		relation := "same_project"
		if i == 1 {
			relation = "cross_project"
		}
		for _, visibility := range []string{"private", "project"} {
			t.Run(relation+"/create_"+visibility, func(t *testing.T) {
				before := snapshot(t)
				_, e := svc.CreateDocument(bounded, docdom.CreateDocumentInput{ProjectID: project, Title: "bounded must not create", Visibility: visibility})
				if e == nil || !strings.Contains(e.Error(), "bounded task grants") {
					t.Errorf("expected explicit grant refusal, got %v", e)
				}
				if after := snapshot(t); after != before {
					t.Error("denied create changed document rows or task associations")
				}
			})
			for _, d := range docs[project] {
				t.Run(relation+"/access_"+d.Visibility+"_to_"+visibility, func(t *testing.T) {
					before := snapshot(t)
					var version int64
					if e := env.db.Get(&version, `SELECT access_version FROM documents WHERE id=$1`, d.ID); e != nil {
						t.Fatal(e)
					}
					_, e := svc.UpdateDocumentAccess(bounded, project, d.ID, docdom.UpdateAccessInput{ExpectedAccessVersion: version, Visibility: visibility})
					if e == nil || !strings.Contains(e.Error(), "bounded task grants") {
						t.Errorf("expected explicit grant refusal, got %v", e)
					}
					if after := snapshot(t); after != before {
						t.Error("denied access mutation changed rows, visibility, version or task associations")
					}
				})
			}
		}
	}
	// A Task2 resolution failure is not itself a document denial: this real
	// human has only docs.read/write, no tasks.read, and can create private docs.
	seedUser(t, env, "grant-docs-only", "password1", "Docs only")
	docsUser, e := env.userRepo.FindByUsername(env.ctx, "grant-docs-only")
	if e != nil {
		t.Fatal(e)
	}
	role := uuid.New()
	if _, e = env.db.Exec(`INSERT INTO project_roles(id,project_id,role_name,permissions)VALUES($1,$2,'Docs only','{"docs.read":true,"docs.write":true}')`, role, projects[1]); e != nil {
		t.Fatal(e)
	}
	if _, e = env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type)VALUES($1,$2,$3,$4,'human')`, uuid.New(), projects[1], docsUser.ID, role); e != nil {
		t.Fatal(e)
	}
	if _, e = authz.NewTaskAccessPolicy(pgRepo.NewAuthzPermissionStore(env.db)).ResolveTaskAccess(authz.WithActorID(env.ctx, docsUser.ID), projects[1]); e == nil {
		t.Fatal("fixture unexpectedly has task read")
	}
	dc, dt := taskMemberLogin(t, env, "grant-docs-only", "password1")
	req := mustRequest(env.ctx, t, "POST", env.base+"/api/v1/projects/"+projects[1].String()+"/docs", jsonBody(t, map[string]any{"title": "normal docs-only private"}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+dt)
	resp := mustDo(t, dc, req)
	defer resp.Body.Close()
	assertStatus(t, resp, 201)
	var result envelope
	decodeJSON(t, resp, &result)
	data := assertDataMap(t, result)
	if data["visibility"] != "private" || data["owner_user_id"] != docsUser.ID.String() {
		t.Fatalf("wrong docs-only private owner: %+v", data)
	}
	waitFileGate(t, env)
}
