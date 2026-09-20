package e2e_test

import (
	"github.com/google/uuid"
	"net/http"
	"testing"
)

// This exact test also runs on BASE: only isolated seed SQL branches on schema.
func TestDocumentLegacyReadContract(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "legacy-owner", "password1")
	client, token := taskMemberLogin(t, env, "legacy-owner", "password1")
	pid := createProjectForTasksViaAPI(t, env, client, token)
	var member uuid.UUID
	if e := env.db.Get(&member, `SELECT id FROM project_members WHERE project_id=$1 LIMIT 1`, pid); e != nil {
		t.Fatal(e)
	}
	seedUser(t, env, "legacy-peer", "password1", "Legacy Peer")
	peer, _ := env.userRepo.FindByUsername(env.ctx, "legacy-peer")
	if _, e := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, uuid.New(), pid, peer.ID); e != nil {
		t.Fatal(e)
	}
	pc, pt := taskMemberLogin(t, env, "legacy-peer", "password1")
	seedUser(t, env, "legacy-outsider", "password1", "Legacy Outsider")
	oc, ot := taskMemberLogin(t, env, "legacy-outsider", "password1")
	agent, agentMember := uuid.New(), uuid.New()
	if _, e := env.db.Exec(`INSERT INTO agents(id,project_id,name,handle,agent_type,agent_scope,acp_provider,llm_provider,llm_model,llm_api_key_secret) VALUES($1,$2,'Legacy Agent','legacy-agent','acp','project','claude-code','','','')`, agent, pid); e != nil {
		t.Fatal(e)
	}
	if _, e := env.db.Exec(`INSERT INTO project_members(id,project_id,agent_id,project_role_id,member_type) SELECT $1,$2,$3,id,'agent' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, agentMember, pid, agent); e != nil {
		t.Fatal(e)
	}
	var migrated bool
	if e := env.db.Get(&migrated, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='documents' AND column_name='visibility')`); e != nil {
		t.Fatal(e)
	}
	var docs []uuid.UUID
	for _, creator := range []*uuid.UUID{&member, &agentMember, nil} {
		id := uuid.New()
		docs = append(docs, id)
		q := `INSERT INTO documents(id,project_id,created_by,title,content) VALUES($1,$2,$3,'legacy title','[]')`
		if migrated {
			q = `INSERT INTO documents(id,project_id,created_by,title,content,visibility) VALUES($1,$2,$3,'legacy title','[]','legacy_project')`
		}
		if _, e := env.db.Exec(q, id, pid, creator); e != nil {
			t.Fatal(e)
		}
	}
	seedUser(t, env, "legacy-writer-only", "password1", "Writer only")
	writer, _ := env.userRepo.FindByUsername(env.ctx, "legacy-writer-only")
	role := uuid.New()
	if _, e := env.db.Exec(`INSERT INTO project_roles(id,project_id,role_name,permissions)VALUES($1,$2,'Writer only','{"docs.write":true}')`, role, pid); e != nil {
		t.Fatal(e)
	}
	if _, e := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type)VALUES($1,$2,$3,$4,'human')`, uuid.New(), pid, writer.ID, role); e != nil {
		t.Fatal(e)
	}
	wc, wt := taskMemberLogin(t, env, "legacy-writer-only", "password1")
	writeReq := mustRequest(env.ctx, t, "PATCH", env.base+"/api/v1/projects/"+pid+"/docs/"+docs[0].String(), jsonBody(t, map[string]any{"title": "legacy write remains permitted"}))
	writeReq.Header.Set("Content-Type", "application/json")
	writeReq.Header.Set("Authorization", "Bearer "+wt)
	writeResp := mustDo(t, wc, writeReq)
	writeResp.Body.Close()
	assertStatus(t, writeResp, 200)
	actors := []struct {
		name   string
		client *http.Client
		token  string
		want   int
	}{{"write-only", wc, wt, 403}, {"owner", client, token, 200}, {"peer", pc, pt, 200}, {"outsider", oc, ot, 403}, {"anonymous", &http.Client{}, "", 401}}
	for _, a := range actors {
		for i, id := range docs {
			req := mustRequest(env.ctx, t, "GET", env.base+"/api/v1/projects/"+pid+"/docs/"+id.String(), nil)
			if a.token != "" {
				req.Header.Set("Authorization", "Bearer "+a.token)
			}
			resp := mustDo(t, a.client, req)
			resp.Body.Close()
			t.Logf("legacy creator[%d] actor=%s status=%d", i, a.name, resp.StatusCode)
			assertStatus(t, resp, a.want)
		}
	}
	req := mustRequest(env.ctx, t, "PATCH", env.base+"/api/v1/projects/"+pid, jsonBody(t, map[string]any{"is_public": true}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp := mustDo(t, client, req)
	resp.Body.Close()
	assertStatus(t, resp, 200)
	for _, id := range docs {
		req = mustRequest(env.ctx, t, "GET", env.base+"/api/v1/projects/"+pid+"/docs/"+id.String(), nil)
		resp = mustDo(t, &http.Client{}, req)
		resp.Body.Close()
		assertStatus(t, resp, 200)
	}
}
