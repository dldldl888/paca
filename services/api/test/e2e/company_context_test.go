package e2e_test

import (
	"context"
	"encoding/json"
	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/delivery"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	docsvc "github.com/Paca-AI/api/internal/service/doc"
	"github.com/Paca-AI/api/internal/transport/http/handler"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompanyContextSelectionPersistenceAndDeletion(t *testing.T) {
	env := newE2EEnv(t)
	client, token, project, role := seedACPUser(t, env)
	pid := uuid.MustParse(project)
	_, result := createAgentRequest(t, env, client, token, project, acpAgentBody(role, "company-context", nil))
	aid := uuid.MustParse(assertDataMap(t, result)["id"].(string))
	var user, member uuid.UUID
	if err := env.db.QueryRow(`SELECT user_id,id FROM project_members WHERE project_id=$1 AND member_type='human' LIMIT 1`, pid).Scan(&user, &member); err != nil {
		t.Fatal(err)
	}
	docRepo := pg.NewDocumentRepository(env.db)
	instance := uuid.New()
	gate := delivery.NewGate()
	env.agentSvc.WithAuthorizer(authz.NewAuthorizer(pg.NewAuthzPermissionStore(env.db))).WithPrivateRuns(instance, env.agentRepo, docsvc.New(docRepo, nil), nil, nil).WithDeliveryGate(gate)
	coordination := pg.NewAuthorizationDelivery(env.db, env.redisClient, instance.String())
	coordination.Reconcile = env.agentSvc.ReconcilePrivateRuns
	stop, err := coordination.StartParticipant(env.ctx, gate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stop(ctx)
	}()
	if err = coordination.Mutate(env.ctx, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for gate.State().Blocked {
		if time.Now().After(deadline) {
			t.Fatal("gate did not open")
		}
		time.Sleep(time.Millisecond)
	}
	coordination.Install()
	folder, child, ordinary, doc, hidden := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, args := range [][]any{{folder, pid, nil, "公司资料"}, {child, pid, folder, "公司资料"}, {ordinary, pid, nil, "其他"}} {
		if _, err = env.db.Exec(`INSERT INTO doc_folders(id,project_id,parent_id,name) VALUES($1,$2,$3,$4)`, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]any{{doc, pid, folder, "正式制度", "合成公司事实"}, {hidden, pid, child, "不应自动读取", "子目录秘密"}, {uuid.New(), pid, ordinary, "其他制度", "其他资料"}} {
		content, _ := json.Marshal([]any{map[string]any{"content": []any{map[string]any{"type": "text", "text": args[4]}}}})
		if _, err = env.db.Exec(`INSERT INTO documents(id,project_id,folder_id,title,content) VALUES($1,$2,$3,$4,$5)`, args[0], args[1], args[2], args[3], content); err != nil {
			t.Fatal(err)
		}
	}
	// A private project with no owner membership and a public project are not sources.
	for _, public := range []bool{false, true} {
		other, otherFolder := uuid.New(), uuid.New()
		if _, err = env.db.Exec(`INSERT INTO projects(id,name,is_public) VALUES($1,$2,$3)`, other, "other-"+other.String(), public); err != nil {
			t.Fatal(err)
		}
		if public {
			if _, err = env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) VALUES($1,$2,$3,$4,'human')`, uuid.New(), other, user, role); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = env.db.Exec(`INSERT INTO doc_folders(id,project_id,name) VALUES($1,$2,'公司资料')`, otherFolder, other); err != nil {
			t.Fatal(err)
		}
		if _, err = env.db.Exec(`INSERT INTO documents(id,project_id,folder_id,title,content) VALUES($1,$2,$3,'外部秘密','[]')`, uuid.New(), other, otherFolder); err != nil {
			t.Fatal(err)
		}
	}
	_, c, err := env.agentSvc.StartChatSession(authz.WithActorID(env.ctx, user), pid, aid, member, "company", nil, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	// Force the final refs write to fail; no source body may be returned.
	if _, err = env.db.Exec(`CREATE FUNCTION reject_company_refs() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF jsonb_array_length(NEW.context_items)>0 THEN RAISE EXCEPTION 'synthetic refs failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_company_refs BEFORE UPDATE ON agent_conversations FOR EACH ROW EXECUTE FUNCTION reject_company_refs()`); err != nil {
		t.Fatal(err)
	}
	if out, e := env.agentSvc.CompanyContext(env.ctx, c.ID, aid); e == nil || out != nil {
		t.Fatal("body returned before durable refs", out, e)
	}
	if _, err = env.db.Exec(`DROP TRIGGER reject_company_refs ON agent_conversations`); err != nil {
		t.Fatal(err)
	}
	out, err := env.agentSvc.CompanyContext(env.ctx, c.ID, aid)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) != 1 || !strings.Contains(out.Text, "合成公司事实") || strings.Contains(out.Text, "秘密") || strings.Contains(out.Text, "其他资料") {
		t.Fatal(out)
	}
	stored, err := env.agentRepo.FindConversationByID(env.ctx, c.ID)
	if err != nil || len(stored.ContextItems) != 1 || stored.ContextItems[0].ID != doc.String() {
		t.Fatal(stored, err)
	}
	h := &handler.AgentRunHandler{Service: env.agentSvc, InternalKey: "synthetic"}
	for _, test := range []struct {
		body, key string
		status    int
	}{{`{"conversation_id":"` + c.ID.String() + `","agent_id":"` + aid.String() + `"}`, "wrong", 401}, {`{"conversation_id":"` + c.ID.String() + `","agent_id":"` + aid.String() + `","owner_user_id":"` + user.String() + `"}`, "synthetic", 400}, {`{"conversation_id":"` + c.ID.String() + `","agent_id":"` + aid.String() + `"}`, "synthetic", 200}} {
		req := httptest.NewRequest(http.MethodPost, "/api/internal/agent-runs/company-context", strings.NewReader(test.body))
		req.Header.Set("X-Internal-Token", test.key)
		rec := httptest.NewRecorder()
		h.CompanyContext(rec, req)
		if rec.Code != test.status {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	// Moving out changes selection, not authorization of already disclosed history.
	if _, err = env.db.Exec(`UPDATE documents SET folder_id=$1 WHERE id=$2`, ordinary, doc); err != nil {
		t.Fatal(err)
	}
	out, err = env.agentSvc.CompanyContext(env.ctx, c.ID, aid)
	if err != nil || len(out.Sources) != 0 {
		t.Fatal(out, err)
	}
	if _, err = env.agentSvc.AuthorizeRun(env.ctx, c.ID, aid, false); err != nil {
		t.Fatal("directory move revoked ordinary read", err)
	}
	// Delete must be visible inside the authority mutation's transaction so
	// reconciliation fails the active run before the gate reopens.
	if err = docRepo.DeleteDocument(env.ctx, doc); err != nil {
		t.Fatal(err)
	}
	stored, err = env.agentRepo.FindConversationByID(env.ctx, c.ID)
	if err != nil || stored.Status != string(agentdom.ConversationStatusFailed) {
		t.Fatalf("deleted source left active run: %+v %v", stored, err)
	}
}
