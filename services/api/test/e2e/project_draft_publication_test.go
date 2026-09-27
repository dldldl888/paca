package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	draftdom "github.com/Paca-AI/api/internal/domain/draft"
	"github.com/Paca-AI/api/internal/platform/authz"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	"github.com/google/uuid"
)

func publicationDraft(t *testing.T, f *draftFixture, owner, session uuid.UUID) map[string]any {
	t.Helper()
	a, b := uuid.NewString(), uuid.NewString()
	return f.request(t, http.MethodPost, f.base, map[string]any{
		"name": "发布回归", "goal": "项目交付", "chat_session_id": session,
		"participants": []any{map[string]any{"user_id": owner.String()}},
		"items": []any{
			map[string]any{"id": a, "title": "后续任务", "requirements": "交付要求", "criteria": "完成标准", "assignee_user_id": owner.String(), "due_date": "2026-12-01T18:00:00+08:00", "importance": 2, "dependencies": []string{b}},
			map[string]any{"id": b, "title": "前置任务", "requirements": "核对来源", "criteria": "通过检查", "assignee_user_id": owner.String(), "due_date": "2026-11-30T18:00:00+08:00", "importance": 3, "dependencies": []string{}},
		},
	}, http.StatusCreated)
}

func TestPrivateProjectDraftPublicationFreezesContent(t *testing.T) {
	f, owner, session, agent := newProjectDraftFixture(t)
	d := publicationDraft(t, f, owner, session)
	path := f.base + "/" + d["id"].(string)
	f.request(t, http.MethodPost, path+"/publish", map[string]any{"expected_version": 1, "publication_id": uuid.New()}, 200)
	frozen := f.request(t, http.MethodGet, path, nil, 200)
	if frozen["state"] != "published" {
		t.Errorf("published draft still mutable: state=%v", frozen["state"])
	}
	preview := f.request(t, http.MethodPost, path+"/preview", map[string]any{"expected_version": 1}, 200)
	if preview["can_publish"] != false {
		t.Fatalf("published draft preview remained publishable: %#v", preview)
	}
	f.request(t, http.MethodPatch, path, map[string]any{"expected_version": 1, "name": "changed"}, 409)
	ctx := authz.WithHumanSession(authz.WithActorID(context.Background(), owner))
	if _, err := pg.NewDraftRepository(f.env.db).StartProjectDraftGeneration(ctx, uuid.MustParse(d["id"].(string)), 1, agent, 0); !errors.Is(err, draftdom.ErrConflict) {
		t.Fatalf("published draft accepted generation: %v", err)
	}
	f.request(t, http.MethodPost, path+"/files/initiate-upload", map[string]any{"file_name": "late.txt", "content_type": "text/plain", "file_size": 5}, 409)
}

func TestPrivateProjectDraftPublicationReceiptSurvivesTaskChanges(t *testing.T) {
	f, owner, session, _ := newProjectDraftFixture(t)
	d := publicationDraft(t, f, owner, session)
	path := f.base + "/" + d["id"].(string)
	input := map[string]any{"expected_version": 1, "publication_id": uuid.New()}
	first := f.request(t, http.MethodPost, path+"/publish", input, 200)
	if _, err := f.env.db.Exec(`UPDATE tasks SET deleted_at=now() WHERE project_id=$1`, first["project_id"]); err != nil {
		t.Fatal(err)
	}
	replay := f.request(t, http.MethodPost, path+"/publish", input, 200)
	if !reflect.DeepEqual(first, replay) {
		t.Fatalf("publication receipt changed with live tasks: first=%v replay=%v", first, replay)
	}
	saved := f.request(t, http.MethodGet, path, nil, 200)
	if !reflect.DeepEqual(saved["publication"], first) {
		t.Fatalf("refresh did not recover receipt: %#v", saved["publication"])
	}
	input["expected_version"] = 2
	f.request(t, http.MethodPost, path+"/publish", input, 409)
}

func TestPrivateProjectDraftPublicationUsesTaskContentContract(t *testing.T) {
	f, owner, session, _ := newProjectDraftFixture(t)
	d := publicationDraft(t, f, owner, session)
	p := f.request(t, http.MethodPost, f.base+"/"+d["id"].(string)+"/publish", map[string]any{"expected_version": 1, "publication_id": uuid.New()}, 200)
	var rows []struct {
		Description json.RawMessage `db:"description"`
		Importance  int             `db:"importance"`
		Custom      json.RawMessage `db:"custom_fields"`
	}
	if err := f.env.db.Select(&rows, `SELECT description,importance,custom_fields FROM tasks WHERE project_id=$1 ORDER BY task_number`, p["project_id"]); err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(rows[0].Description, &blocks); err != nil {
		t.Fatalf("employee renderer requires BlockNote array: %s", rows[0].Description)
	}
	if len(blocks) == 0 || rows[0].Importance != 2 || rows[1].Importance != 3 {
		t.Fatalf("lost task description or priority: %#v", rows)
	}
	var custom map[string]string
	if err := json.Unmarshal(rows[0].Custom, &custom); err != nil {
		t.Fatal(err)
	}
	if custom["draft_due_at"] != "2026-12-01T18:00:00+08:00" {
		t.Fatalf("lost original due timestamp: %#v", custom)
	}
	var links, activities int
	if err := f.env.db.Get(&links, `SELECT count(*) FROM task_links WHERE target_task_id IN (SELECT id FROM tasks WHERE project_id=$1)`, p["project_id"]); err != nil {
		t.Fatal(err)
	}
	if err := f.env.db.Get(&activities, `SELECT count(*) FROM task_activities WHERE task_id IN (SELECT id FROM tasks WHERE project_id=$1)`, p["project_id"]); err != nil {
		t.Fatal(err)
	}
	if links != 1 || activities != 2 {
		t.Fatalf("missing dependency/audit: links=%d activities=%d", links, activities)
	}
}

func TestPrivateProjectDraftPublicationConcurrentReplay(t *testing.T) {
	f, owner, session, _ := newProjectDraftFixture(t)
	d := publicationDraft(t, f, owner, session)
	ctx := authz.WithHumanSession(authz.WithActorID(context.Background(), owner))
	repo := pg.NewDraftRepository(f.env.db)
	id, pub := uuid.MustParse(d["id"].(string)), uuid.New()
	before := projectBusinessCounts(t, f.env)
	var wg sync.WaitGroup
	results := make([]*draftdom.ProjectDraftPublication, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = repo.PublishProjectDraft(ctx, id, 1, pub) }(i)
	}
	wg.Wait()
	created := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if results[i].Created {
			created++
		}
		if results[0].PublicationID != results[i].PublicationID || results[0].ProjectID != results[i].ProjectID || !reflect.DeepEqual(results[0].TaskIDs, results[i].TaskIDs) {
			t.Fatalf("concurrent replay public result diverged: %v", results)
		}
	}
	if created != 1 {
		t.Fatalf("expected exactly one creating publish result, got %d", created)
	}
	after := projectBusinessCounts(t, f.env)
	if after[0] != before[0]+1 || after[2] != before[2]+2 || after[5] != before[5]+2 {
		t.Fatalf("concurrent duplicate business writes: %v -> %v", before, after)
	}
}

func TestPrivateProjectDraftPublicationRejectsInactiveParticipant(t *testing.T) {
	f, owner, session, _ := newProjectDraftFixture(t)
	seedUser(t, f.env, "inactive-draft-person", "password1", "Inactive")
	inactive, err := f.env.userRepo.FindByUsername(f.env.ctx, "inactive-draft-person")
	if err != nil {
		t.Fatal(err)
	}
	d := f.request(t, http.MethodPost, f.base, map[string]any{
		"name": "停用人员", "goal": "拒绝发布", "chat_session_id": session,
		"participants": []any{map[string]any{"user_id": inactive.ID.String()}},
		"items":        []any{map[string]any{"id": uuid.NewString(), "title": "任务", "requirements": "要求", "criteria": "标准", "assignee_user_id": inactive.ID.String(), "due_date": "2026-12-01T18:00:00+08:00", "importance": 1, "dependencies": []string{}}},
	}, http.StatusCreated)
	if _, err := f.env.db.Exec(`UPDATE users SET deleted_at=now() WHERE id=$1`, inactive.ID); err != nil {
		t.Fatal(err)
	}
	f.request(t, http.MethodPost, f.base+"/"+d["id"].(string)+"/publish", map[string]any{"expected_version": 1, "publication_id": uuid.New()}, http.StatusBadRequest)
	_ = owner
}

func TestPrivateProjectDraftPreviewReportsInactiveParticipant(t *testing.T) {
	f, _, session, _ := newProjectDraftFixture(t)
	seedUser(t, f.env, "inactive-preview-person", "password1", "Inactive preview")
	inactive, err := f.env.userRepo.FindByUsername(f.env.ctx, "inactive-preview-person")
	if err != nil {
		t.Fatal(err)
	}
	d := f.request(t, http.MethodPost, f.base, map[string]any{
		"name": "停用预览", "goal": "预览时提示", "chat_session_id": session,
		"participants": []any{map[string]any{"user_id": inactive.ID.String()}},
		"items": []any{map[string]any{
			"id": uuid.NewString(), "title": "任务", "requirements": "要求", "criteria": "标准",
			"assignee_user_id": inactive.ID.String(), "due_date": "2026-12-01T18:00:00+08:00", "dependencies": []string{},
		}},
	}, http.StatusCreated)
	if _, err := f.env.db.Exec(`UPDATE users SET deleted_at=now() WHERE id=$1`, inactive.ID); err != nil {
		t.Fatal(err)
	}
	preview := f.request(t, http.MethodPost, f.base+"/"+d["id"].(string)+"/preview", map[string]any{"expected_version": 1}, http.StatusOK)
	if preview["can_publish"] != false {
		t.Fatalf("preview should block inactive participant: %#v", preview)
	}
	found := false
	for _, raw := range preview["issues"].([]any) {
		issue := raw.(map[string]any)
		if strings.Contains(issue["message"].(string), "停用") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("preview did not explain inactive participant: %#v", preview)
	}
}

func TestPrivateProjectDraftPublicationRequiresManagerInPreviewParticipants(t *testing.T) {
	f, owner, session, _ := newProjectDraftFixture(t)
	seedUser(t, f.env, "unselected-manager", "password1", "Manager")
	manager, err := f.env.userRepo.FindByUsername(f.env.ctx, "unselected-manager")
	if err != nil {
		t.Fatal(err)
	}
	f.request(t, http.MethodPost, f.base, map[string]any{
		"name": "未选负责人", "goal": "预览必须一致", "chat_session_id": session,
		"manager_user_id": manager.ID.String(),
		"participants":    []any{map[string]any{"user_id": owner.String(), "role": "执行人"}},
		"items":           []any{map[string]any{"id": uuid.NewString(), "title": "任务", "requirements": "要求", "criteria": "标准", "assignee_user_id": owner.String(), "due_date": "2026-12-01T18:00:00+08:00", "importance": 1, "dependencies": []string{}}},
	}, http.StatusBadRequest)
}

func TestPrivateProjectDraftPublicationRollsBackEveryStage(t *testing.T) {
	for _, table := range []string{"projects", "project_roles", "project_members", "task_types", "task_statuses", "task_counters", "tasks", "task_assignees", "task_activities", "notifications", "task_links", "project_drafts"} {
		t.Run(table, func(t *testing.T) {
			f, owner, session, _ := newProjectDraftFixture(t)
			d := publicationDraft(t, f, owner, session)
			before := publicationWriteCounts(t, f.env)
			if _, err := f.env.db.Exec(`CREATE FUNCTION reject_publication_stage() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected publication failure'; END $$`); err != nil {
				t.Fatal(err)
			}
			event := "INSERT"
			if table == "project_drafts" {
				event = "UPDATE"
			}
			if _, err := f.env.db.Exec(`CREATE TRIGGER reject_publication_stage BEFORE ` + event + ` ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION reject_publication_stage()`); err != nil {
				t.Fatal(err)
			}
			path := f.base + "/" + d["id"].(string)
			input := map[string]any{"expected_version": 1, "publication_id": uuid.New()}
			f.request(t, http.MethodPost, path+"/publish", input, http.StatusInternalServerError)
			if after := publicationWriteCounts(t, f.env); !reflect.DeepEqual(before, after) {
				t.Fatalf("partial publication at %s: %v -> %v", table, before, after)
			}
			current := f.request(t, http.MethodGet, path, nil, http.StatusOK)
			if current["state"] != "editable" || current["publication"] != nil || current["publication_id"] != nil {
				t.Fatalf("failed publication froze draft or left receipt: %#v", current)
			}
			if _, err := f.env.db.Exec(`DROP TRIGGER reject_publication_stage ON ` + table); err != nil {
				t.Fatal(err)
			}
			f.request(t, http.MethodPost, path+"/publish", input, http.StatusOK)
		})
	}
}

func publicationWriteCounts(t *testing.T, env *e2eEnv) []int {
	t.Helper()
	counts := make([]int, 0, 12)
	for _, table := range []string{"projects", "project_roles", "project_members", "task_types", "task_statuses", "task_counters", "tasks", "task_assignees", "task_activities", "notifications", "task_links", "project_drafts"} {
		var n int
		if err := env.db.Get(&n, "SELECT count(*) FROM "+table); err != nil {
			t.Fatal(err)
		}
		counts = append(counts, n)
	}
	return counts
}
