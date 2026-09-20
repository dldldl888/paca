package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	notificationdom "github.com/Paca-AI/api/internal/domain/notification"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/delivery"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	docsvc "github.com/Paca-AI/api/internal/service/doc"
	draftsvc "github.com/Paca-AI/api/internal/service/draft"
	notificationsvc "github.com/Paca-AI/api/internal/service/notification"
	"github.com/google/uuid"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type draftFixture struct {
	env         *e2eEnv
	client      *http.Client
	token       string
	pid, member uuid.UUID
	base        string
}

func newDraftFixture(t *testing.T) *draftFixture {
	t.Helper()
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "draft-manager", "password1")
	c, token := taskMemberLogin(t, env, "draft-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, c, token))
	var member uuid.UUID
	if err := env.db.Get(&member, "SELECT id FROM project_members WHERE project_id=$1 AND member_type='human' LIMIT 1", pid); err != nil {
		t.Fatal(err)
	}
	return &draftFixture{env, c, token, pid, member, "/api/v1/projects/" + pid.String() + "/drafts"}
}
func (f *draftFixture) request(t *testing.T, method, path string, body any, want int) map[string]any {
	t.Helper()
	req := mustRequest(f.env.ctx, t, method, f.env.base+path, nil)
	if body != nil {
		req = mustRequest(f.env.ctx, t, method, f.env.base+path, jsonBody(t, body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp := mustDo(t, f.client, req)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s: want %d got %d: %s", method, path, want, resp.StatusCode, b)
	}
	var e envelope
	if err = json.Unmarshal(b, &e); err != nil {
		t.Fatal(err, string(b))
	}
	if want >= 400 {
		return map[string]any{"error_code": e.ErrorCode}
	}
	return assertDataMap(t, e)
}
func (f *draftFixture) content() map[string]any {
	a, b := uuid.NewString(), uuid.NewString()
	item := func(id string, deps []string) map[string]any {
		return map[string]any{"id": id, "title": "合成任务 " + id, "requirements": "公开交付", "criteria": "公开验收", "assignee_member_id": f.member.String(), "due_date": "2026-12-31T23:59:00+08:00", "start_date": "", "importance": 2, "dependencies": deps}
	}
	return map[string]any{"goal": "PRIVATE_GOAL", "sources": []string{"PRIVATE_SOURCE"}, "assumptions": []string{"PRIVATE_ASSUMPTION"}, "items": []any{item(a, []string{}), item(b, []string{a})}}
}
func (f *draftFixture) counts(t *testing.T) []int64 {
	t.Helper()
	out := []int64{}
	for _, table := range []string{"tasks", "task_assignees", "task_links", "task_activities", "notifications"} {
		var n int64
		if err := f.env.db.Get(&n, "SELECT count(*) FROM "+table); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	var n int64
	if err := f.env.db.Get(&n, "SELECT coalesce(sum(last_value),0) FROM task_counters"); err != nil {
		t.Fatal(err)
	}
	return append(out, n)
}
func TestDraftLifecycleAndEmployeeProjection(t *testing.T) {
	f := newDraftFixture(t)
	before := f.counts(t)
	body := f.content()
	d := f.request(t, "POST", f.base, body, 201)
	path := f.base + "/" + d["id"].(string)
	if d["version"] != float64(1) {
		t.Fatal(d)
	}
	p := f.request(t, "POST", path+"/preview", map[string]any{"expected_version": 1}, 200)
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "PRIVATE_") || p["can_publish"] != true {
		t.Fatal(string(b))
	}
	if !reflect.DeepEqual(before, f.counts(t)) {
		t.Fatal("save/preview created formal data")
	}
	body["expected_version"] = 1
	d = f.request(t, "PATCH", path, body, 200)
	f.request(t, "PATCH", path, body, 409)
	pub := map[string]any{"expected_version": 2, "publication_id": uuid.NewString(), "confirmed": true}
	result := f.request(t, "POST", path+"/publish", pub, 200)
	replay := f.request(t, "POST", path+"/publish", pub, 200)
	if !reflect.DeepEqual(result["published_tasks"], replay["published_tasks"]) {
		t.Fatal("duplicate result")
	}
	if result["state"] != "published" {
		t.Fatal(result)
	}
	if got := f.counts(t); !reflect.DeepEqual(got, []int64{2, 2, 1, 2, 2, 2}) {
		t.Fatal(got)
	}
	var leaked int
	if err := f.env.db.Get(&leaked, "SELECT count(*) FROM tasks WHERE description::text LIKE '%PRIVATE_%'"); err != nil || leaked != 0 {
		t.Fatal(leaked, err)
	}
	body["expected_version"] = 2
	f.request(t, "PATCH", path, body, 409)
	pub["publication_id"] = uuid.NewString()
	f.request(t, "POST", path+"/publish", pub, 409)
}
func TestPublishRollsBackEveryWriteStage(t *testing.T) {
	f := newDraftFixture(t)
	for _, table := range []string{"tasks", "task_assignees", "task_links", "task_activities", "notifications", "task_drafts"} {
		t.Run(table, func(t *testing.T) {
			d := f.request(t, "POST", f.base, f.content(), 201)
			path := f.base + "/" + d["id"].(string)
			before := f.counts(t)
			pub := map[string]any{"expected_version": 1, "publication_id": uuid.NewString(), "confirmed": true}
			condition := "TRUE"
			if table == "tasks" {
				condition = fmt.Sprintf("NEW.task_number = %d", before[5]+2)
			}
			if table == "task_drafts" {
				condition = "NEW.state = 'published'"
			}
			sql := fmt.Sprintf("CREATE OR REPLACE FUNCTION reject_draft_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF %s THEN RAISE EXCEPTION 'synthetic stage failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_draft_write BEFORE INSERT OR UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION reject_draft_write()", condition, table)
			if _, err := f.env.db.Exec(sql); err != nil {
				t.Fatal(err)
			}
			f.request(t, "POST", path+"/publish", pub, 500)
			if !reflect.DeepEqual(before, f.counts(t)) {
				t.Fatalf("partial data: before %v after %v", before, f.counts(t))
			}
			unchanged := f.request(t, "GET", path, nil, 200)
			if !reflect.DeepEqual(d, unchanged) {
				t.Fatal(unchanged)
			}
			if _, err := f.env.db.Exec("DROP TRIGGER reject_draft_write ON " + table); err != nil {
				t.Fatal(err)
			}
			first := f.request(t, "POST", path+"/publish", pub, 200)
			second := f.request(t, "POST", path+"/publish", pub, 200)
			if !reflect.DeepEqual(first["published_tasks"], second["published_tasks"]) {
				t.Fatal("retry duplicated")
			}
		})
	}
}
func TestPublishConcurrentAndLostResponse(t *testing.T) {
	f := newDraftFixture(t)
	for _, mode := range []string{"same", "different", "cross-draft"} {
		t.Run(mode, func(t *testing.T) {
			first := f.request(t, "POST", f.base, f.content(), 201)
			second := first
			if mode == "cross-draft" {
				second = f.request(t, "POST", f.base, f.content(), 201)
			}
			paths := []string{f.base + "/" + first["id"].(string) + "/publish", f.base + "/" + second["id"].(string) + "/publish"}
			ids := []string{uuid.NewString(), ""}
			ids[1] = ids[0]
			if mode == "different" {
				ids[1] = uuid.NewString()
			}
			codes := make([]int, 2)
			results := make([]map[string]any, 2)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range 2 {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					req := mustRequest(f.env.ctx, t, "POST", f.env.base+paths[i], jsonBody(t, map[string]any{"expected_version": 1, "publication_id": ids[i], "confirmed": true}))
					req.Header.Set("Authorization", "Bearer "+f.token)
					req.Header.Set("Content-Type", "application/json")
					resp := mustDo(t, f.client, req)
					defer resp.Body.Close()
					codes[i] = resp.StatusCode
					var e envelope
					decodeJSON(t, resp, &e)
					if e.Success {
						results[i] = assertDataMap(t, e)
					}
				}(i)
			}
			close(start)
			wg.Wait()
			if mode == "same" {
				if codes[0] != 200 || codes[1] != 200 || !reflect.DeepEqual(results[0]["published_tasks"], results[1]["published_tasks"]) {
					t.Fatal(codes, results)
				}
			} else if !(codes[0] == 200 && codes[1] == 409 || codes[1] == 200 && codes[0] == 409) {
				t.Fatal(codes, results)
			}
			winner := 0
			if codes[0] != 200 {
				winner = 1
			}
			replay := f.request(t, "POST", paths[winner], map[string]any{"expected_version": 1, "publication_id": ids[winner], "confirmed": true}, 200)
			if !reflect.DeepEqual(results[winner]["published_tasks"], replay["published_tasks"]) {
				t.Fatal("lost response replay")
			}
		})
	}
	if got := f.counts(t); !reflect.DeepEqual(got, []int64{6, 6, 3, 6, 6, 6}) {
		t.Fatal(got)
	}
}

func TestDraftAccessAndHumanPublishOnly(t *testing.T) {
	f := newDraftFixture(t)
	d := f.request(t, "POST", f.base, f.content(), 201)
	path := f.base + "/" + d["id"].(string)
	f.request(t, "POST", f.base, map[string]any{"owner_user_id": uuid.NewString()}, 400)
	f.request(t, "PATCH", path, map[string]any{"expected_version": 1, "project_id": uuid.NewString()}, 400)
	f.request(t, "POST", path+"/generate", map[string]any{"expected_version": 1, "agent_id": uuid.NewString(), "conversation_id": uuid.NewString()}, 400)
	f.request(t, "POST", path+"/import", map[string]any{"expected_version": 1, "generation_id": uuid.NewString(), "items": []any{}}, 400)
	pub := map[string]any{"expected_version": 1, "publication_id": uuid.NewString(), "confirmed": false}
	f.request(t, "POST", path+"/publish", pub, 400)
	// RequireJWTAuth by itself rejects keys, but OptionalAuthn permits anonymous;
	// the draft group must also require actual authentication.
	req := mustRequest(f.env.ctx, t, "POST", f.env.base+path+"/publish", jsonBody(t, pub))
	req.Header.Set("Content-Type", "application/json")
	resp := mustDo(t, &http.Client{}, req)
	resp.Body.Close()
	assertStatus(t, resp, 401)
	_, key := createAPIKey(t, f.env, f.client, f.token, "draft-human-key")
	req = mustRequest(f.env.ctx, t, "POST", f.env.base+path+"/publish", jsonBody(t, pub))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "ApiKey "+key)
	resp = mustDo(t, &http.Client{}, req)
	resp.Body.Close()
	assertStatus(t, resp, 403)
	for _, name := range []string{"other-manager", "employee", "global-admin"} {
		t.Run(name, func(t *testing.T) {
			seedUser(t, f.env, name, "password1", name)
			u, e := f.env.userRepo.FindByUsername(f.env.ctx, name)
			if e != nil {
				t.Fatal(e)
			}
			role := "Admin"
			if name == "employee" {
				role = "Editor"
			}
			if name == "global-admin" {
				assignGlobalRolesByName(t, f.env, name, "ADMIN")
			}
			{
				if _, e = f.env.db.Exec("INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name=$4", uuid.New(), f.pid, u.ID, role); e != nil {
					t.Fatal(e)
				}
			}
			c, token := taskMemberLogin(t, f.env, name, "password1")
			other := *f
			other.client, other.token = c, token
			for _, route := range []struct {
				method, suffix string
				body           any
			}{{"GET", "", nil}, {"PATCH", "", map[string]any{"expected_version": 1}}, {"POST", "/preview", map[string]any{"expected_version": 1}}, {"POST", "/publish", map[string]any{"expected_version": 1, "publication_id": uuid.NewString(), "confirmed": true}}} {
				other.request(t, route.method, path+route.suffix, route.body, 404)
			}
			if name == "employee" {
				other.request(t, "GET", f.base, nil, 403)
			} else {
				list := other.request(t, "GET", f.base, nil, 200)
				if len(list["items"].([]any)) != 0 {
					t.Fatal("owner list leak")
				}
			}
		})
	}
	if got := f.counts(t); !reflect.DeepEqual(got, []int64{0, 0, 0, 0, 0, 0}) {
		t.Fatal(got)
	}
}

func TestPublishRevalidatesCurrentAuthorityAndMembers(t *testing.T) {
	for _, scenario := range []string{"manager_role", "manager_account", "assignee_removed", "assignee_account", "cross_project", "agent_assignee"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDraftFixture(t)
			body := f.content()
			items := body["items"].([]any)
			seedUser(t, f.env, "recipient", "password1", "recipient")
			recipient, e := f.env.userRepo.FindByUsername(f.env.ctx, "recipient")
			if e != nil {
				t.Fatal(e)
			}
			mid := uuid.New()
			if _, e = f.env.db.Exec("INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'", mid, f.pid, recipient.ID); e != nil {
				t.Fatal(e)
			}
			for _, it := range items {
				it.(map[string]any)["assignee_member_id"] = mid.String()
			}
			d := f.request(t, "POST", f.base, body, 201)
			path := f.base + "/" + d["id"].(string)
			f.request(t, "POST", path+"/preview", map[string]any{"expected_version": 1}, 200)
			want := 400
			switch scenario {
			case "manager_role":
				_, e = f.env.db.Exec("UPDATE project_roles SET permissions='{}' WHERE project_id=$1 AND role_name='Admin'", f.pid)
				want = 403
			case "manager_account":
				_, e = f.env.db.Exec("UPDATE users SET deleted_at=now() WHERE id=(SELECT user_id FROM project_members WHERE id=$1)", f.member)
				want = 404
			case "assignee_removed":
				_, e = f.env.db.Exec("UPDATE project_members SET deleted_at=now() WHERE id=$1", mid)
			case "assignee_account":
				_, e = f.env.db.Exec("UPDATE users SET deleted_at=now() WHERE id=$1", recipient.ID)
			case "cross_project":
				other := uuid.MustParse(createProjectForTasksViaAPI(t, f.env, f.client, f.token))
				_, e = f.env.db.Exec("UPDATE project_members SET project_id=$1 WHERE id=$2", other, mid)
			case "agent_assignee":
				role := projectRoleIDByName(t, f.env, f.client, f.token, f.pid.String(), "Editor")
				_, result := createAgentRequest(t, f.env, f.client, f.token, f.pid.String(), acpAgentBody(role, "draft-agent", nil))
				aid := uuid.MustParse(assertDataMap(t, result)["id"].(string))
				var agentMember uuid.UUID
				if e = f.env.db.Get(&agentMember, "SELECT id FROM project_members WHERE agent_id=$1", aid); e != nil {
					t.Fatal(e)
				}
				body["expected_version"] = 1
				for _, it := range items {
					it.(map[string]any)["assignee_member_id"] = agentMember.String()
				}
				d = f.request(t, "PATCH", path, body, 200)
			}
			if e != nil {
				t.Fatal(e)
			}
			f.request(t, "POST", path+"/publish", map[string]any{"expected_version": d["version"], "publication_id": uuid.NewString(), "confirmed": true}, want)
			if got := f.counts(t); !reflect.DeepEqual(got, []int64{0, 0, 0, 0, 0, 0}) {
				t.Fatal(got)
			}
		})
	}
}

func TestDraftVersionAndGenerationRace(t *testing.T) {
	f := newDraftFixture(t)
	env := f.env
	role := projectRoleIDByName(t, env, f.client, f.token, f.pid.String(), "Editor")
	status, result := createAgentRequest(t, env, f.client, f.token, f.pid.String(), acpAgentBody(role, "hermes", map[string]any{"acp_provider": "custom", "acp_command": []string{"synthetic-test-command"}, "parallelism_limit": 1}))
	if status != 201 {
		t.Fatal(status, result)
	}
	aid := uuid.MustParse(assertDataMap(t, result)["id"].(string))
	env.agentSvc.WithAuthorizer(authz.NewAuthorizer(pg.NewAuthzPermissionStore(env.db))).WithPrivateRuns(uuid.New(), env.agentRepo, nil, nil, nil)
	d := f.request(t, "POST", f.base, f.content(), 201)
	path := f.base + "/" + d["id"].(string)
	generated := f.request(t, "POST", path+"/generate", map[string]any{"expected_version": 1, "agent_id": aid}, 200)
	g := generated["generation"].(map[string]any)
	if g["status"] != "running" || generated["version"] != float64(2) {
		t.Fatal(generated)
	}
	cid := uuid.MustParse(g["conversation_id"].(string))
	sid := uuid.MustParse(g["chat_session_id"].(string))
	session, e := env.agentRepo.FindChatSessionByID(env.ctx, sid)
	if e != nil || session.MemberID != f.member {
		t.Fatal("generation owner", session, e)
	}
	f.request(t, "POST", path+"/import", map[string]any{"expected_version": 2, "generation_id": g["id"]}, 409)
	// Only model-visible events from the bound completed first turn are importable.
	complete := func(cid uuid.UUID, model string) {
		t.Helper()
		for i, ev := range []struct {
			kind, source string
			payload      map[string]any
		}{
			{"user_message", "user", map[string]any{"content": map[string]any{"type": "text", "text": "synthetic request"}}},
			{"agent_thought_chunk", "agent", map[string]any{"content": map[string]any{"type": "text", "text": "PRIVATE_THOUGHT not JSON"}}},
			{"agent_message_chunk", "agent", map[string]any{"content": map[string]any{"type": "text", "text": model}}},
			{"turn_end", "system", map[string]any{"stopReason": "end_turn"}},
		} {
			if err := env.agentRepo.CreateConversationEvent(env.ctx, &agentdom.AgentConversationEvent{ID: uuid.New(), ConversationID: cid, EventIndex: i, EventType: ev.kind, EventSource: ev.source, Payload: ev.payload, CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
		if err := env.agentRepo.UpdateConversationStatus(env.ctx, cid, "finished"); err != nil {
			t.Fatal(err)
		}
	}
	content := f.content()
	model, _ := json.Marshal(map[string]any{"items": content["items"], "sources": []string{}, "assumptions": []string{}})
	complete(cid, string(model))
	imported := f.request(t, "POST", path+"/import", map[string]any{"expected_version": 2, "generation_id": g["id"]}, 200)
	if imported["version"] != float64(3) || imported["goal"] != "PRIVATE_GOAL" || imported["generation"].(map[string]any)["status"] != "imported" {
		t.Fatal(imported)
	}
	// A new generation must use a fresh conversation and cannot overwrite saved edits.
	generated = f.request(t, "POST", path+"/generate", map[string]any{"expected_version": 3, "agent_id": aid}, 200)
	g = generated["generation"].(map[string]any)
	next := uuid.MustParse(g["conversation_id"].(string))
	if next == cid {
		t.Fatal("reused previous round")
	}
	content["expected_version"] = 4
	content["goal"] = "HUMAN_EDIT"
	f.request(t, "PATCH", path, content, 200)
	complete(next, string(model))
	f.request(t, "POST", path+"/import", map[string]any{"expected_version": 4, "generation_id": g["id"]}, 409)
	f.request(t, "POST", path+"/import", map[string]any{"expected_version": 5, "generation_id": g["id"]}, 409)
	current := f.request(t, "GET", path, nil, 200)
	if current["goal"] != "HUMAN_EDIT" {
		t.Fatal("late generation overwrote edit")
	}
	// Cross-draft import has no caller-provided source link to spoof.
	other := f.request(t, "POST", f.base, f.content(), 201)
	f.request(t, "POST", f.base+"/"+other["id"].(string)+"/import", map[string]any{"expected_version": 1, "generation_id": g["id"]}, 409)
	generated = f.request(t, "POST", path+"/generate", map[string]any{"expected_version": 5, "agent_id": aid}, 200)
	g = generated["generation"].(map[string]any)
	complete(uuid.MustParse(g["conversation_id"].(string)), `{"items":[],"owner_user_id":"model"}`)
	failed := f.request(t, "POST", path+"/import", map[string]any{"expected_version": 6, "generation_id": g["id"]}, 200)
	if failed["version"] != float64(6) || failed["generation"].(map[string]any)["status"] != "failed" || failed["goal"] != "HUMAN_EDIT" {
		t.Fatal(failed)
	}
	if got := f.counts(t); !reflect.DeepEqual(got, []int64{0, 0, 0, 0, 0, 0}) {
		t.Fatal(got)
	}
}

func TestNotificationReplayRecoveryAndRevocation(t *testing.T) {
	f := newDraftFixture(t)
	env := f.env
	seedUser(t, env, "notify-recipient", "password1", "Recipient")
	recipientClient, recipientToken := taskMemberLogin(t, env, "notify-recipient", "password1")
	recipient, e := env.userRepo.FindByUsername(env.ctx, "notify-recipient")
	if e != nil {
		t.Fatal(e)
	}
	mid := uuid.New()
	if _, e = env.db.Exec("INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'", mid, f.pid, recipient.ID); e != nil {
		t.Fatal(e)
	}
	var owner uuid.UUID
	if e = env.db.Get(&owner, "SELECT user_id FROM project_members WHERE id=$1", f.member); e != nil {
		t.Fatal(e)
	}
	task, e := env.taskSvc.CreateTask(authz.WithActorID(env.ctx, owner), taskdom.CreateTaskInput{ProjectID: f.pid, Title: "PRIVATE_TASK_SUMMARY", AssigneeIDs: []uuid.UUID{mid}})
	if e != nil {
		t.Fatal(e)
	}
	repo := pg.NewNotificationRepository(env.db)
	svc := notificationsvc.New(repo, env.projectRepo, nil).WithCurrentDelivery(nil, env.taskSvc)
	in := notificationdom.NotifyAssignedInput{TaskID: task.ID, ProjectID: f.pid, NewAssigneeMemberID: mid, ActorUserID: owner, SourceEventID: "paca:task:assignments:123-0"}
	for range 2 {
		if e = svc.NotifyAssigned(env.ctx, in); e != nil {
			t.Fatal(e)
		}
	}
	rows, _, e := repo.ListForUser(env.ctx, recipient.ID, 20, nil)
	if e != nil || len(rows) != 1 || rows[0].TaskTitle != "PRIVATE_TASK_SUMMARY" {
		t.Fatal(rows, e)
	}
	in.SourceEventID = "paca:task:assignments:124-0"
	if e = svc.NotifyAssigned(env.ctx, in); e != nil {
		t.Fatal(e)
	}
	rows, _, e = repo.ListForUser(env.ctx, recipient.ID, 20, nil)
	if e != nil || len(rows) != 2 {
		t.Fatal(rows, e)
	}
	if e = repo.MarkAsRead(env.ctx, rows[0].ID, owner); !errors.Is(e, notificationdom.ErrNotificationNotFound) {
		t.Fatal("other user marked read", e)
	}
	var adminRole, editorRole uuid.UUID
	if e = env.db.Get(&adminRole, "SELECT id FROM project_roles WHERE project_id=$1 AND role_name='Admin'", f.pid); e != nil {
		t.Fatal(e)
	}
	if e = env.db.Get(&editorRole, "SELECT id FROM project_roles WHERE project_id=$1 AND role_name='Editor'", f.pid); e != nil {
		t.Fatal(e)
	}
	if _, e = env.db.Exec("UPDATE project_members SET project_role_id=$1 WHERE id=$2", adminRole, mid); e != nil {
		t.Fatal(e)
	}
	creatorTask, e := env.taskSvc.CreateTask(authz.WithActorID(env.ctx, recipient.ID), taskdom.CreateTaskInput{ProjectID: f.pid, Title: "CREATOR_ONLY_NOTIFICATION", AssigneeIDs: []uuid.UUID{mid}})
	if e != nil {
		t.Fatal(e)
	}
	creatorTaskID := creatorTask.ID
	if e = repo.Create(env.ctx, &notificationdom.Notification{ID: uuid.New(), RecipientUserID: recipient.ID, Type: notificationdom.NotificationTypeMentioned, TaskID: &creatorTaskID, ProjectID: f.pid, CreatedAt: time.Now()}); e != nil {
		t.Fatal(e)
	}
	if _, e = env.db.Exec("UPDATE project_members SET project_role_id=$1 WHERE id=$2", editorRole, mid); e != nil {
		t.Fatal(e)
	}
	if _, e = env.db.Exec("DELETE FROM task_assignees WHERE task_id IN ($1,$2)", task.ID, creatorTask.ID); e != nil {
		t.Fatal(e)
	}
	rows, _, e = repo.ListForUser(env.ctx, recipient.ID, 20, nil)
	if e != nil || len(rows) != 3 {
		t.Fatal(e)
	}
	for _, row := range rows {
		if row.TaskID != nil || row.TaskTitle != "" || row.TaskNumber != 0 || row.ProjectID != f.pid || row.ProjectName == "" {
			t.Fatal("revoked task projection leaked or hid current project", row)
		}
	}
	request := func() []any {
		t.Helper()
		req := mustRequest(env.ctx, t, http.MethodGet, env.base+"/api/v1/users/me/notifications", nil)
		req.Header.Set("Authorization", "Bearer "+recipientToken)
		resp := mustDo(t, recipientClient, req)
		defer resp.Body.Close()
		assertStatus(t, resp, http.StatusOK)
		var result envelope
		decodeJSON(t, resp, &result)
		return assertDataMap(t, result)["items"].([]any)
	}
	for _, raw := range request() {
		item := raw.(map[string]any)
		if item["task_id"] != nil || item["task_title"] != "" || item["task_number"] != float64(0) || item["project_id"] != f.pid.String() || item["project_name"] == "" {
			t.Fatal("revoked notification API projection", item)
		}
	}
	if _, e = env.db.Exec("UPDATE project_members SET deleted_at=now() WHERE id=$1", mid); e != nil {
		t.Fatal(e)
	}
	rows, _, e = repo.ListForUser(env.ctx, recipient.ID, 20, nil)
	if e != nil || len(rows) != 3 {
		t.Fatal(rows, e)
	}
	for _, row := range rows {
		if row.TaskID != nil || row.ProjectID != uuid.Nil || row.ProjectName != "" {
			t.Fatal("former project notification leaked navigation", row)
		}
	}
	for _, raw := range request() {
		item := raw.(map[string]any)
		if item["task_id"] != nil || item["project_id"] != nil || item["project_name"] != "" {
			t.Fatal("former project notification API leaked navigation", item)
		}
	}
}

func TestPublishLocksCurrentAuthorityAndMembers(t *testing.T) {
	for _, mode := range []string{"role", "assignee"} {
		t.Run(mode, func(t *testing.T) {
			f := newDraftFixture(t)
			env := f.env
			body := f.content()
			seedUser(t, env, "locked-recipient", "password1", "Recipient")
			recipient, e := env.userRepo.FindByUsername(env.ctx, "locked-recipient")
			if e != nil {
				t.Fatal(e)
			}
			mid := uuid.New()
			if _, e = env.db.Exec("INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'", mid, f.pid, recipient.ID); e != nil {
				t.Fatal(e)
			}
			for _, it := range body["items"].([]any) {
				it.(map[string]any)["assignee_member_id"] = mid.String()
			}
			d := f.request(t, "POST", f.base, body, 201)
			did := uuid.MustParse(d["id"].(string))
			var owner uuid.UUID
			if e = env.db.Get(&owner, "SELECT user_id FROM project_members WHERE id=$1", f.member); e != nil {
				t.Fatal(e)
			}
			ctx := authz.WithActorID(env.ctx, owner)
			repo := pg.NewDraftRepository(env.db)
			// Hold an uncommitted revocation. Publish must wait for these exact rows,
			// then resolve the committed state rather than using an earlier snapshot.
			tx, e := env.db.BeginTxx(env.ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback()
			if mode == "role" {
				_, e = tx.Exec("UPDATE project_roles SET permissions='{}' WHERE project_id=$1 AND role_name='Admin'", f.pid)
			} else {
				_, e = tx.Exec("UPDATE users SET deleted_at=now() WHERE id=$1", recipient.ID)
			}
			if e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() { _, e := repo.Publish(ctx, f.pid, did, 1, uuid.New()); done <- e }()
			waitForDraftLock(t, env)
			if e = tx.Commit(); e != nil {
				t.Fatal(e)
			}
			select {
			case e = <-done:
				if e == nil {
					t.Fatal("published after revocation")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("publish did not resolve")
			}
			if got := f.counts(t); !reflect.DeepEqual(got, []int64{0, 0, 0, 0, 0, 0}) {
				t.Fatal(got)
			}
			// Trusted automation grants and agent identities also cannot call repository publish.
			if mode == "role" {
				return
			}
			grant, e := authz.WithTaskServiceGrant(context.Background(), authz.TaskServiceGrant{Name: "bounded", ProjectID: f.pid, TaskIDs: []uuid.UUID{uuid.New()}, Action: taskdom.AccessManage})
			if e != nil {
				t.Fatal(e)
			}
			for _, actorCtx := range []context.Context{grant, authz.WithAgentCredential(ctx), authz.WithAgentID(ctx, uuid.New())} {
				if _, e = repo.Publish(actorCtx, f.pid, did, 1, uuid.New()); e == nil {
					t.Fatal("non-human service context published")
				}
			}
		})
	}
}
func waitForDraftLock(t *testing.T, env *e2eEnv) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		if e := env.db.Get(&blocked, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND pid<>pg_backend_pid())"); e != nil {
			t.Fatal(e)
		}
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("publish did not wait on current authority/member lock")
}

func draftHermes(t *testing.T, f *draftFixture) uuid.UUID {
	t.Helper()
	role := projectRoleIDByName(t, f.env, f.client, f.token, f.pid.String(), "Editor")
	status, res := createAgentRequest(t, f.env, f.client, f.token, f.pid.String(), acpAgentBody(role, "hermes", map[string]any{"acp_provider": "custom", "acp_command": []string{"synthetic-test-command"}, "parallelism_limit": 1}))
	if status != 201 {
		t.Fatal(status, res)
	}
	return uuid.MustParse(assertDataMap(t, res)["id"].(string))
}
func completeDraftGeneration(t *testing.T, f *draftFixture, cid uuid.UUID) {
	t.Helper()
	c := f.content()
	model, _ := json.Marshal(map[string]any{"items": c["items"], "sources": []string{}, "assumptions": []string{}})
	for i, ev := range []struct {
		kind, source string
		payload      map[string]any
	}{
		{"user_message", "user", map[string]any{"content": map[string]any{"type": "text", "text": "synthetic"}}},
		{"agent_message_chunk", "agent", map[string]any{"content": map[string]any{"type": "text", "text": string(model)}}},
		{"turn_end", "system", map[string]any{"stopReason": "end_turn"}},
	} {
		if e := f.env.agentRepo.CreateConversationEvent(f.env.ctx, &agentdom.AgentConversationEvent{ID: uuid.New(), ConversationID: cid, EventIndex: i, EventType: ev.kind, EventSource: ev.source, Payload: ev.payload, CreatedAt: time.Now()}); e != nil {
			t.Fatal(e)
		}
	}
	if e := f.env.agentRepo.UpdateConversationStatus(f.env.ctx, cid, "finished"); e != nil {
		t.Fatal(e)
	}
}
func TestDraftGenerationStartCannotModifyPublishedOrEditedDraft(t *testing.T) {
	for _, action := range []string{"publish", "edit"} {
		t.Run(action, func(t *testing.T) {
			f := newDraftFixture(t)
			env := f.env
			aid := draftHermes(t, f)
			d := f.request(t, "POST", f.base, f.content(), 201)
			path := f.base + "/" + d["id"].(string)
			// Block session insertion after the generation reservation, without production hooks.
			lock, e := env.db.BeginTxx(env.ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer lock.Rollback()
			if _, e = lock.Exec("SELECT pg_advisory_xact_lock(914102)"); e != nil {
				t.Fatal(e)
			}
			if _, e = env.db.Exec("CREATE FUNCTION pause_draft_session() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(914102); RETURN NEW; END $$; CREATE TRIGGER pause_draft_session BEFORE INSERT ON agent_chat_sessions FOR EACH ROW EXECUTE FUNCTION pause_draft_session()"); e != nil {
				t.Fatal(e)
			}
			var owner uuid.UUID
			if e = env.db.Get(&owner, "SELECT user_id FROM project_members WHERE id=$1", f.member); e != nil {
				t.Fatal(e)
			}
			svc := draftsvc.New(pg.NewDraftRepository(env.db), env.agentSvc, env.projectRepo)
			done := make(chan error, 1)
			go func() {
				_, e := svc.Generate(authz.WithActorID(env.ctx, owner), f.pid, uuid.MustParse(d["id"].(string)), 1, aid)
				done <- e
			}()
			waitForDraftLock(t, env)
			if action == "publish" {
				f.request(t, "POST", path+"/publish", map[string]any{"expected_version": 2, "publication_id": uuid.NewString(), "confirmed": true}, 200)
			} else {
				body := f.content()
				body["expected_version"] = 2
				body["goal"] = "human newer edit"
				f.request(t, "PATCH", path, body, 200)
			}
			before := f.request(t, "GET", path, nil, 200)
			if e = lock.Commit(); e != nil {
				t.Fatal(e)
			}
			select {
			case e = <-done:
				if e == nil {
					t.Fatal("late generation modified saved draft")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("generation stuck")
			}
			after := f.request(t, "GET", path, nil, 200)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("late generation changed immutable saved version")
			}
		})
	}
}
func TestDraftImportFencesCrossProjectRevocation(t *testing.T) {
	f := newDraftFixture(t)
	env := f.env
	aid := draftHermes(t, f)
	instance := uuid.New()
	other := uuid.MustParse(createProjectForTasksViaAPI(t, env, f.client, f.token))
	doc := uuid.New()
	if _, e := env.db.Exec("INSERT INTO documents(id,project_id,title,content) VALUES($1,$2,'private source','[]')", doc, other); e != nil {
		t.Fatal(e)
	}
	env.agentSvc.WithAuthorizer(authz.NewAuthorizer(pg.NewAuthzPermissionStore(env.db))).WithPrivateRuns(instance, env.agentRepo, docsvc.New(pg.NewDocumentRepository(env.db), nil), nil, nil)
	repo := pg.NewDraftRepository(env.db)
	gate := delivery.NewGate()
	svc := draftsvc.New(repo, env.agentSvc, env.projectRepo).WithDeliveryGate(gate)
	var owner uuid.UUID
	if e := env.db.Get(&owner, "SELECT user_id FROM project_members WHERE id=$1", f.member); e != nil {
		t.Fatal(e)
	}
	ctx := authz.WithActorID(env.ctx, owner)
	d := f.request(t, "POST", f.base, f.content(), 201)
	did := uuid.MustParse(d["id"].(string))
	path := f.base + "/" + did.String()
	gen := f.request(t, "POST", path+"/generate", map[string]any{"expected_version": 1, "agent_id": aid}, 200)["generation"].(map[string]any)
	gid, cid := uuid.MustParse(gen["id"].(string)), uuid.MustParse(gen["conversation_id"].(string))
	otherID := other.String()
	if e := env.agentRepo.MergeConversationContext(env.ctx, cid, []agentdom.ContextItemRef{{Type: agentdom.ContextItemDoc, ID: doc.String(), ProjectID: &otherID}}); e != nil {
		t.Fatal(e)
	}
	completeDraftGeneration(t, f, cid)
	if _, e := svc.Import(ctx, f.pid, did, 2, gid); !errors.Is(e, delivery.ErrBlocked) {
		t.Fatalf("closed gate must deny import before transaction: %v", e)
	}
	// Use the real coordination barrier for a foreign-project source deletion.
	coordinator := pg.NewAuthorizationDelivery(env.db, env.redisClient, instance.String())
	coordinator.Reconcile = env.agentSvc.ReconcilePrivateRuns
	env.agentSvc.WithDeliveryGate(gate)
	stop, e := coordinator.StartParticipant(env.ctx, gate)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stop(c)
	}()
	if e = coordinator.Mutate(env.ctx, nil); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(5 * time.Second)
	for gate.State().Blocked && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if gate.State().Blocked {
		t.Fatal("gate did not reopen")
	}
	lock, e := env.db.BeginTxx(env.ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Rollback()
	if _, e = lock.Exec("SELECT pg_advisory_xact_lock(914103)"); e != nil {
		t.Fatal(e)
	}
	if _, e = env.db.Exec("CREATE FUNCTION pause_draft_import() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.content->'generation'->>'status'='imported' THEN PERFORM pg_advisory_xact_lock(914103); END IF; RETURN NEW; END $$; CREATE TRIGGER pause_draft_import BEFORE UPDATE ON task_drafts FOR EACH ROW EXECUTE FUNCTION pause_draft_import()"); e != nil {
		t.Fatal(e)
	}
	imported := make(chan error, 1)
	go func() { _, e := svc.Import(ctx, f.pid, did, 2, gid); imported <- e }()
	waitForDraftLock(t, env)
	revoked := make(chan error, 1)
	go func() {
		revoked <- coordinator.Mutate(env.ctx, func(ctx context.Context) error {
			_, e := pg.DeliveryQuery(ctx, env.db).ExecContext(ctx, "UPDATE documents SET deleted_at=now() WHERE id=$1", doc)
			return e
		})
	}()
	deadline = time.Now().Add(5 * time.Second)
	for !gate.State().Blocked && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !gate.State().Blocked {
		t.Fatal("revocation did not close gate")
	}
	select {
	case e = <-revoked:
		t.Fatalf("revocation passed active import lease: %v", e)
	default:
	}
	var deleted bool
	if e = env.db.Get(&deleted, "SELECT deleted_at IS NOT NULL FROM documents WHERE id=$1", doc); e != nil || deleted {
		t.Fatal("source revoked before import committed", e)
	}
	if e = lock.Commit(); e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-imported:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("import stuck")
	}
	select {
	case e = <-revoked:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revocation stuck")
	}
	// The committed import linearizes before revocation; its completion never trails an acknowledged revoke.
	current, e := repo.Get(ctx, f.pid, did)
	if e != nil || current.Version != 3 {
		t.Fatal(current, e)
	}
}

func TestDraftPublishDurableBeforeRefresh(t *testing.T) {
	f := newDraftFixture(t)
	env := f.env
	d := f.request(t, "POST", f.base, f.content(), 201)
	path := f.base + "/" + d["id"].(string)
	sub := env.redisClient.Subscribe(env.ctx, "paca.events")
	defer sub.Close()
	if _, e := sub.Receive(env.ctx); e != nil {
		t.Fatal(e)
	}
	pub := map[string]any{"expected_version": 1, "publication_id": uuid.NewString(), "confirmed": true}
	f.request(t, "POST", path+"/publish", pub, 200)
	ctx, cancel := context.WithTimeout(env.ctx, time.Second)
	defer cancel()
	message, e := sub.ReceiveMessage(ctx)
	if e != nil {
		t.Fatal("missing post-commit notification refresh", e)
	}
	if strings.Contains(message.Payload, "PRIVATE_") {
		t.Fatal("private background in refresh")
	}
	if !strings.Contains(message.Payload, "notification.created") {
		t.Fatal(message.Payload)
	}
	if got := f.counts(t); !reflect.DeepEqual(got, []int64{2, 2, 1, 2, 2, 2}) {
		t.Fatal("refresh preceded durable commit", got)
	}
	second := f.request(t, "POST", f.base, f.content(), 201)
	if e = env.redisClient.Close(); e != nil {
		t.Fatal(e)
	}
	f.request(t, "POST", f.base+"/"+second["id"].(string)+"/publish", map[string]any{"expected_version": 1, "publication_id": uuid.NewString(), "confirmed": true}, 200)
	if got := f.counts(t); !reflect.DeepEqual(got, []int64{4, 4, 2, 4, 4, 4}) {
		t.Fatal("failed refresh damaged durable publication", got)
	}

}

func TestDraftConcurrentEditsUseExpectedVersion(t *testing.T) {
	f := newDraftFixture(t)
	d := f.request(t, "POST", f.base, f.content(), 201)
	path := f.base + "/" + d["id"].(string)
	start := make(chan struct{})
	codes := make(chan int, 2)
	for _, goal := range []string{"first tab", "second tab"} {
		go func(goal string) {
			body := f.content()
			body["goal"] = goal
			body["expected_version"] = 1
			req := mustRequest(f.env.ctx, t, "PATCH", f.env.base+path, jsonBody(t, body))
			req.Header.Set("Authorization", "Bearer "+f.token)
			req.Header.Set("Content-Type", "application/json")
			<-start
			resp := mustDo(t, f.client, req)
			defer resp.Body.Close()
			codes <- resp.StatusCode
		}(goal)
	}
	close(start)
	a, b := <-codes, <-codes
	if !(a == 200 && b == 409 || a == 409 && b == 200) {
		t.Fatal(a, b)
	}
	current := f.request(t, "GET", path, nil, 200)
	if current["version"] != float64(2) {
		t.Fatal(current)
	}
	if got := f.counts(t); !reflect.DeepEqual(got, []int64{0, 0, 0, 0, 0, 0}) {
		t.Fatal(got)
	}
}
