package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	"github.com/Paca-AI/api/internal/platform/authz"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	docsvc "github.com/Paca-AI/api/internal/service/doc"
	"github.com/google/uuid"
)

// Real HTTP + PostgreSQL; only initial fixtures/model output are synthetic.
// Removing the historical source guard must expose the persisted sentinel again.
func TestHistorySourceRevocation(t *testing.T) {
	env := newE2EEnv(t)
	client, token, project, role := seedACPUser(t, env)
	pid := uuid.MustParse(project)
	_, result := createAgentRequest(t, env, client, token, project, acpAgentBody(role, "history-source", nil))
	aid := uuid.MustParse(assertDataMap(t, result)["id"].(string))
	seedUser(t, env, "history-agent-bot", "password1", "Bot")
	assignGlobalRolesByName(t, env, "history-agent-bot", "SUPER_ADMIN")
	bot, _ := env.userRepo.FindByUsername(env.ctx, "history-agent-bot")
	agentKey := "paca_history_test_" + uuid.NewString()
	env.apiKeySvc.WithAgentKey(agentKey, bot.ID).WithAgentIdentityStore(env.agentRepo)
	var member, user, humanRole uuid.UUID
	if err := env.db.QueryRow(`SELECT id,user_id,project_role_id FROM project_members WHERE project_id=$1 AND member_type='human' LIMIT 1`, pid).Scan(&member, &user, &humanRole); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := env.db.Exec(q, args...); e != nil {
			t.Fatal(e)
		}
	}
	global := uuid.New()
	exec(`INSERT INTO agents(id,name,handle,agent_type,agent_scope,acp_provider,llm_provider,llm_model,llm_api_key_secret) VALUES($1,'Global','history-global','acp','global','claude-code','','','')`, global)
	source := uuid.New()
	const secret = "HISTORYSOURCECANARY"
	exec(`INSERT INTO documents(id,project_id,title,content,visibility,owner_user_id)VALUES($1,$2,$3,'[]','private',$4)`, source, pid, secret, user)
	authorizer := authz.NewAuthorizer(pg.NewAuthzPermissionStore(env.db))
	env.agentSvc.WithAuthorizer(authorizer).WithPrivateRuns(uuid.New(), env.agentRepo, docsvc.New(pg.NewDocumentRepository(env.db), env.projectRepo), nil, env.automationSvc).WithDeliveryGate(env.fileGate)
	coordinator := startFileDelivery(t, env)
	coordinator.Reconcile = env.agentSvc.ReconcilePrivateRuns
	if e := coordinator.Mutate(env.ctx, nil); e != nil {
		t.Fatal(e)
	}
	waitFileGate(t, env)
	p := pid.String()
	refs := []agentdom.ContextItemRef{{Type: agentdom.ContextItemDoc, ID: source.String(), ProjectID: &p, Title: secret}}
	type fixture struct {
		c            *agentdom.AgentConversation
		base, cursor string
	}
	makeHistory := func(globalScope bool, status string, contextItems []agentdom.ContextItemRef) fixture {
		t.Helper()
		now := time.Now()
		marker := secret
		if contextItems != nil && len(contextItems) == 0 {
			marker = "readable control history"
		}
		title := "control"
		if len(contextItems) > 0 {
			title = secret
		}
		session := &agentdom.AgentChatSession{ID: uuid.New(), AgentID: aid, ProjectID: pid, MemberID: member, Title: &title, CreatedAt: now, UpdatedAt: now}
		if globalScope {
			session.AgentID = global
			session.ProjectID = uuid.Nil
			session.MemberID = uuid.Nil
			session.ActorUserID = &user
		}
		if e := env.agentRepo.CreateChatSession(env.ctx, session); e != nil {
			t.Fatal(e)
		}
		c := &agentdom.AgentConversation{ID: uuid.New(), AgentID: session.AgentID, ProjectID: session.ProjectID, ChatSessionID: &session.ID, ActorUserID: session.ActorUserID, Audience: agentdom.AudienceOwnerPrivate, TriggerType: "chat_message", Status: status, ContextItems: contextItems, ErrorMessage: &marker, CreatedAt: now, UpdatedAt: now}
		if e := env.agentRepo.CreateConversation(env.ctx, c); e != nil {
			t.Fatal(e)
		}
		var cursor string
		for i := 0; i < 3; i++ {
			ev := &agentdom.AgentConversationEvent{ID: uuid.New(), ConversationID: c.ID, EventIndex: i, EventType: "agent_message_chunk", EventSource: "agent", Payload: map[string]any{"content": map[string]any{"type": "text", "text": marker}}, CreatedAt: now.Add(time.Duration(i) * time.Second)}
			if e := env.agentRepo.CreateConversationEvent(env.ctx, ev); e != nil {
				t.Fatal(e)
			}
			if i == 1 {
				cursor = agentdom.EncodeConversationEventCursor(ev)
			}
		}
		base := "/api/v1/projects/" + project + "/conversations/" + c.ID.String()
		if globalScope {
			base = "/api/v1/agents/conversations/" + c.ID.String()
		}
		return fixture{c, base, cursor}
	}
	read := func(path string, want int) string {
		t.Helper()
		req := mustRequest(env.ctx, t, "GET", env.base+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp := mustDo(t, client, req)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Errorf("GET %s got=%d want=%d body=%s", path, resp.StatusCode, want, raw)
		}
		if want != 200 && strings.Contains(string(raw), secret) {
			t.Errorf("denial disclosed source: %s", raw)
		}
		return string(raw)
	}
	agentRead := func(h fixture, want int) {
		t.Helper()
		for _, suffix := range []string{"", "/events?limit=1"} {
			req := mustRequest(env.ctx, t, http.MethodGet, env.base+"/api/v1/agents/me/conversations/"+h.c.ID.String()+suffix, nil)
			req.Header.Set("Authorization", "ApiKey "+agentKey)
			req.Header.Set("X-Agent-ID", h.c.AgentID.String())
			req.Header.Set("X-Conversation-ID", h.c.ID.String())
			resp := mustDo(t, &http.Client{}, req)
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != want {
				t.Errorf("agent GET %s got=%d want=%d body=%s", suffix, resp.StatusCode, want, raw)
			}
			if want != http.StatusOK && strings.Contains(string(raw), secret) {
				t.Errorf("agent denial disclosed source: %s", raw)
			}
		}
	}
	chatSessions := func(path string, wantSecret bool) {
		t.Helper()
		body := read(path, http.StatusOK)
		if strings.Contains(body, secret) != wantSecret {
			t.Errorf("GET %s sentinel presence=%v want=%v body=%s", path, strings.Contains(body, secret), wantSecret, body)
		}
	}
	histories := []fixture{}
	for _, globalScope := range []bool{false, true} {
		for _, status := range []string{"finished", "failed", "stopped"} {
			histories = append(histories, makeHistory(globalScope, status, refs))
		}
	}
	nestedID := histories[3].c.ID.String()
	histories = append(histories, makeHistory(true, "finished", []agentdom.ContextItemRef{{Type: agentdom.ContextItemConversation, ID: nestedID}}))
	projectControls := []fixture{makeHistory(false, "finished", []agentdom.ContextItemRef{}), makeHistory(false, "finished", []agentdom.ContextItemRef{})}
	globalControls := []fixture{makeHistory(true, "finished", []agentdom.ContextItemRef{}), makeHistory(true, "finished", []agentdom.ContextItemRef{})}
	foreignSource := uuid.New()
	exec(`INSERT INTO documents(id,project_id,title,content,visibility,owner_user_id)VALUES($1,$2,'foreign private source','[]','private',$3)`, foreignSource, pid, bot.ID)
	foreignRef := []agentdom.ContextItemRef{{Type: agentdom.ContextItemDoc, ID: foreignSource.String(), ProjectID: &p}}
	foreignHistory := makeHistory(true, "finished", foreignRef)
	var roles envelope
	req := mustRequest(env.ctx, t, http.MethodGet, env.base+"/api/v1/projects/"+project+"/roles", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := mustDo(t, client, req)
	decodeJSON(t, resp, &roles)
	resp.Body.Close()
	for _, item := range roles.Data.([]any) {
		r := item.(map[string]any)
		if r["id"] != humanRole.String() {
			continue
		}
		permissions := r["permissions"].(map[string]any)
		delete(permissions, string(authz.PermissionConversationsAll))
		delete(permissions, string(authz.PermissionConversationsWrite))
		permissions[string(authz.PermissionConversationsRead)] = true
		body, _ := json.Marshal(map[string]any{"role_name": r["role_name"], "permissions": permissions})
		req = mustRequest(env.ctx, t, http.MethodPatch, env.base+"/api/v1/projects/"+project+"/roles/"+humanRole.String(), bytes.NewBuffer(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp = mustDo(t, client, req)
		assertStatus(t, resp, http.StatusOK)
		resp.Body.Close()
		break
	}
	waitFileGate(t, env)
	actor := authz.WithActorID(env.ctx, user)
	canRead, err := authorizer.HasPermissions(actor, user, &pid, "", authz.PermissionConversationsRead)
	if err != nil || !canRead {
		t.Fatalf("history fixture lacks conversations.read: allowed=%v err=%v", canRead, err)
	}
	canWrite, err := authorizer.HasPermissions(actor, user, &pid, "", authz.PermissionConversationsWrite)
	if err != nil || canWrite {
		t.Fatalf("history fixture still has conversations.write: allowed=%v err=%v", canWrite, err)
	}
	for _, h := range histories {
		read(h.base, 200)
		for _, query := range []string{"", "?limit=1", "?after=" + url.QueryEscape(h.cursor), "?before=" + url.QueryEscape(h.cursor)} {
			read(h.base+"/events"+query, 200)
		}
	}
	agentRead(histories[0], http.StatusOK)
	agentRead(histories[len(histories)-1], http.StatusOK)
	chatSessions("/api/v1/projects/"+project+"/agents/"+aid.String()+"/chat-sessions", true)
	chatSessions("/api/v1/agents/"+global.String()+"/chat-sessions", true)
	for path, controls := range map[string][]fixture{
		"/api/v1/agents/conversations":                   globalControls,
		"/api/v1/projects/" + project + "/conversations": projectControls,
	} {
		if body := read(path+"?search="+secret+"&page_size=1", http.StatusOK); !strings.Contains(body, secret) || strings.Contains(body, `"items":[]`) {
			t.Fatalf("source search fixture was not indexed before revocation: %s", body)
		}
		if body := read(path+"?search=readable&page_size=1", http.StatusOK); !strings.Contains(body, controls[0].c.ID.String()) && !strings.Contains(body, controls[1].c.ID.String()) {
			t.Fatalf("control search fixture was not indexed before revocation: %s", body)
		}
	}
	unknown := makeHistory(true, "finished", nil)
	read(unknown.base, 404)
	read(foreignHistory.base, 404)
	headerEntered := make(chan int, 1)
	headerRelease, flushEntered, flushRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var headerOnce, flushOnce sync.Once
	releaseHeader := func() { headerOnce.Do(func() { close(headerRelease) }) }
	releaseFlush := func() { flushOnce.Do(func() { close(flushRelease) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.handler.ServeHTTP(&heldTaskJSON{ResponseWriter: w, headerEntered: headerEntered, headerRelease: headerRelease, flushEntered: flushEntered, flushRelease: flushRelease}, r)
	}))
	defer func() { releaseHeader(); releaseFlush(); server.Close() }()
	type httpResult struct {
		status int
		body   string
		err    error
	}
	delivered := make(chan httpResult, 1)
	req = mustRequest(env.ctx, t, http.MethodGet, server.URL+histories[0].base, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	go func() {
		response, e := server.Client().Do(req)
		if e != nil {
			delivered <- httpResult{err: e}
			return
		}
		raw, e := io.ReadAll(response.Body)
		response.Body.Close()
		delivered <- httpResult{status: response.StatusCode, body: string(raw), err: e}
	}()
	select {
	case status := <-headerEntered:
		if status != http.StatusOK {
			t.Fatalf("authorized history read status=%d, want 200", status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("history read never reached WriteHeader")
	}
	revoked := make(chan httpResult, 1)
	deleteRequest := mustRequest(env.ctx, t, http.MethodDelete, fmt.Sprintf("%s/api/v1/projects/%s/docs/%s", env.base, pid, source), nil)
	deleteRequest.Header.Set("Authorization", "Bearer "+token)
	go func() {
		response, e := client.Do(deleteRequest)
		if e != nil {
			revoked <- httpResult{err: e}
			return
		}
		raw, e := io.ReadAll(response.Body)
		response.Body.Close()
		revoked <- httpResult{status: response.StatusCode, body: string(raw), err: e}
	}()
	deadline := time.After(3 * time.Second)
	for !env.fileGate.State().Blocked {
		select {
		case early := <-revoked:
			t.Fatalf("source revocation committed before WriteHeader: %+v", early)
		case <-deadline:
			t.Fatal("source revocation never entered coordinator barrier")
		case <-time.After(time.Millisecond):
		}
	}
	var sourceActive bool
	if err = env.db.Get(&sourceActive, `SELECT deleted_at IS NULL FROM documents WHERE id=$1`, source); err != nil || !sourceActive {
		t.Fatalf("source changed before WriteHeader: active=%v err=%v", sourceActive, err)
	}
	releaseHeader()
	select {
	case <-flushEntered:
	case early := <-revoked:
		t.Fatalf("source revocation committed before Flush: %+v", early)
	case <-time.After(3 * time.Second):
		t.Fatal("history response never reached Flush")
	}
	select {
	case early := <-revoked:
		t.Fatalf("source revocation committed before Flush release: %+v", early)
	default:
	}
	if err = env.db.Get(&sourceActive, `SELECT deleted_at IS NULL FROM documents WHERE id=$1`, source); err != nil || !sourceActive {
		t.Fatalf("source changed before Flush release: active=%v err=%v", sourceActive, err)
	}
	releaseFlush()
	select {
	case old := <-delivered:
		if old.err != nil || old.status != http.StatusOK || !strings.Contains(old.body, secret) {
			t.Fatalf("legal pre-revocation history was not delivered: %+v", old)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("history response did not finish after Flush release")
	}
	select {
	case result := <-revoked:
		if result.err != nil || result.status != http.StatusNoContent {
			t.Fatalf("source revocation failed after Flush: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("source revocation did not commit after Flush")
	}
	waitFileGate(t, env)
	read(foreignHistory.base, 404)
	for _, h := range histories {
		read(h.base, 404)
		for _, query := range []string{"", "?limit=1", "?after=" + url.QueryEscape(h.cursor), "?before=" + url.QueryEscape(h.cursor)} {
			read(h.base+"/events"+query, 404)
		}
	}
	for _, controls := range [][]fixture{projectControls, globalControls} {
		for _, control := range controls {
			read(control.base, 200)
			read(control.base+"/events", 200)
		}
	}
	agentRead(histories[0], http.StatusNotFound)
	agentRead(histories[len(histories)-1], http.StatusNotFound)
	chatSessions("/api/v1/projects/"+project+"/agents/"+aid.String()+"/chat-sessions", false)
	chatSessions("/api/v1/agents/"+global.String()+"/chat-sessions", false)
	// Metadata and event search can also disclose the source, even without events.
	for _, path := range []string{"/api/v1/agents/conversations", "/api/v1/projects/" + project + "/conversations"} {
		body := read(path+"?search="+secret+"&page_size=1", 200)
		if strings.Contains(body, secret) {
			t.Errorf("search disclosed revoked source sentinel: %s", body)
		}
		for _, h := range histories {
			if strings.Contains(body, h.c.ID.String()) {
				t.Errorf("search disclosed revoked history: %s", body)
			}
		}
	}
	for path, controls := range map[string][]fixture{
		"/api/v1/agents/conversations":                   globalControls,
		"/api/v1/projects/" + project + "/conversations": projectControls,
	} {
		allowed := map[string]bool{controls[0].c.ID.String(): true, controls[1].c.ID.String(): true}
		for _, search := range []string{"", "&search=readable"} {
			firstBody := read(path+"?page_size=1"+search, http.StatusOK)
			var first struct {
				Data struct {
					Items []struct {
						ID string `json:"id"`
					} `json:"items"`
					NextCursor *string `json:"next_cursor"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(firstBody), &first); err != nil || len(first.Data.Items) != 1 || !allowed[first.Data.Items[0].ID] || first.Data.NextCursor == nil || strings.Contains(firstBody, secret) {
				t.Fatalf("first readable control page invalid: body=%s err=%v", firstBody, err)
			}
			secondBody := read(path+"?page_size=1"+search+"&cursor="+url.QueryEscape(*first.Data.NextCursor), http.StatusOK)
			var second struct {
				Data struct {
					Items []struct {
						ID string `json:"id"`
					} `json:"items"`
					NextCursor *string `json:"next_cursor"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(secondBody), &second); err != nil || len(second.Data.Items) != 1 || !allowed[second.Data.Items[0].ID] || second.Data.Items[0].ID == first.Data.Items[0].ID || second.Data.NextCursor != nil || strings.Contains(secondBody, secret) {
				t.Fatalf("second readable control page invalid: body=%s err=%v", secondBody, err)
			}
		}
	}
	for _, h := range histories {
		var n int
		if e := env.db.Get(&n, `SELECT count(*) FROM agent_conversation_events WHERE conversation_id=$1`, h.c.ID); e != nil || n != 3 {
			t.Errorf("history was erased: count=%d err=%v", n, e)
		}
	}
}
