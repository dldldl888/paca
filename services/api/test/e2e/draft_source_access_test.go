package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	"github.com/Paca-AI/api/internal/platform/authz"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	agentsvc "github.com/Paca-AI/api/internal/service/agent"
	docsvc "github.com/Paca-AI/api/internal/service/doc"
	"github.com/google/uuid"
)

// The Hermes completion is simulated input. HTTP, PostgreSQL, company-context
// collection, import authorization, and source revocation use production code.
func TestTask4bImportedDraftRetainsSourceAuthorization(t *testing.T) {
	f := newDraftFixture(t)
	env := f.env
	aid := draftHermes(t, f)
	other := uuid.MustParse(createProjectForTasksViaAPI(t, env, f.client, f.token))
	owner, err := env.userRepo.FindByUsername(env.ctx, "draft-manager")
	if err != nil {
		t.Fatal(err)
	}
	instance := uuid.New()
	docs := docsvc.New(pg.NewDocumentRepository(env.db), env.projectRepo)
	env.agentSvc.WithAuthorizer(authz.NewAuthorizer(pg.NewAuthzPermissionStore(env.db))).WithPrivateRuns(instance, env.agentRepo, docs, nil, nil).WithDeliveryGate(env.fileGate)
	coordinator := pg.NewAuthorizationDelivery(env.db, env.redisClient, instance.String())
	coordinator.Reconcile = env.agentSvc.ReconcilePrivateRuns
	stop, err := coordinator.StartParticipant(env.ctx, env.fileGate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stop(ctx); err != nil {
			t.Errorf("participant stop: %v", err)
		}
	})
	coordinator.Install()
	if err := coordinator.Mutate(env.ctx, nil); err != nil {
		t.Fatal(err)
	}
	waitFileGate(t, env)

	raw := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var reader *bytes.Buffer
		if body != nil {
			reader = jsonBody(t, body)
		}
		req := mustRequest(env.ctx, t, method, env.base+path, reader)
		req.Header.Set("Authorization", "Bearer "+f.token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp := mustDo(t, f.client, req)
		defer resp.Body.Close()
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, payload
	}
	data := func(status int, payload []byte) map[string]any {
		t.Helper()
		if status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("unexpected response %d: %s", status, payload)
		}
		var out envelope
		if err := json.Unmarshal(payload, &out); err != nil {
			t.Fatal(err)
		}
		return assertDataMap(t, out)
	}
	openState := func(label string) pg.DeliveryState {
		t.Helper()
		waitFileGate(t, env)
		state, err := coordinator.State(env.ctx)
		if err != nil || state.Blocked || env.fileGate.State().Blocked {
			t.Fatalf("%s barrier not open: state=%+v gate=%+v err=%v", label, state, env.fileGate.State(), err)
		}
		return state
	}

	sentinel := "TASK4B_DERIVED_" + uuid.NewString()
	docBase := "/api/v1/projects/" + other.String() + "/docs"
	folder := f.request(t, http.MethodPost, docBase+"/folders", map[string]any{"name": "公司资料"}, http.StatusCreated)
	source := f.request(t, http.MethodPost, docBase, map[string]any{
		"title": "source", "visibility": "project", "folder_id": folder["id"],
		"content": []any{map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": sentinel}}}},
	}, http.StatusCreated)
	sourceID := source["id"].(string)
	sourcePath := docBase + "/" + sourceID
	status, payload := raw(http.MethodGet, sourcePath, nil)
	if status != http.StatusOK || !strings.Contains(string(payload), sentinel) {
		t.Fatalf("source control: %d %s", status, payload)
	}

	draft := f.request(t, http.MethodPost, f.base, f.content(), http.StatusCreated)
	draftID := draft["id"].(string)
	path := f.base + "/" + draftID
	generated := f.request(t, http.MethodPost, path+"/generate", map[string]any{"expected_version": 1, "agent_id": aid}, http.StatusOK)
	generation := generated["generation"].(map[string]any)
	conversationID := uuid.MustParse(generation["conversation_id"].(string))
	company, err := env.agentSvc.CompanyContext(env.ctx, conversationID, aid)
	if err != nil || !strings.Contains(company.Text, sentinel) {
		t.Fatalf("company context: %+v %v", company, err)
	}
	stored, err := env.agentRepo.FindConversationByID(env.ctx, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ref := range stored.ContextItems {
		found = found || ref.ID == sourceID
	}
	if !found {
		t.Fatal("source context was not persisted")
	}

	content := f.content()
	for _, item := range content["items"].([]any) {
		item.(map[string]any)["title"] = sentinel
		item.(map[string]any)["requirements"] = "derived from " + sentinel
	}
	model, err := json.Marshal(map[string]any{"items": content["items"], "sources": []string{sourceID}, "assumptions": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	for i, event := range []struct {
		kind, source string
		payload      map[string]any
	}{
		{"user_message", "user", map[string]any{"content": map[string]any{"type": "text", "text": "SIMULATED MODEL INPUT"}}},
		{"agent_message_chunk", "agent", map[string]any{"content": map[string]any{"type": "text", "text": string(model)}}},
		{"turn_end", "system", map[string]any{"stopReason": "end_turn"}},
	} {
		if err := env.agentRepo.CreateConversationEvent(env.ctx, &agentdom.AgentConversationEvent{ID: uuid.New(), ConversationID: conversationID, EventIndex: i, EventType: event.kind, EventSource: event.source, Payload: event.payload, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := env.agentRepo.UpdateConversationStatus(env.ctx, conversationID, "finished"); err != nil {
		t.Fatal(err)
	}
	status, payload = raw(http.MethodPost, path+"/import", map[string]any{"expected_version": 2, "generation_id": generation["id"]})
	imported := data(status, payload)
	if imported["version"] != float64(3) || !strings.Contains(string(payload), sentinel) {
		t.Fatalf("import: %s", payload)
	}

	// Client display fields are not trusted provenance. Clearing Sources and
	// starting a replacement generation must preserve the original dependency.
	content["expected_version"] = 3
	content["sources"] = []string{}
	status, payload = raw(http.MethodPatch, path, content)
	patched := data(status, payload)
	if patched["version"] != float64(4) || !strings.Contains(string(payload), sentinel) {
		t.Fatalf("patch control: %s", payload)
	}
	status, payload = raw(http.MethodPost, path+"/generate", map[string]any{"expected_version": 4, "agent_id": aid})
	regenerated := data(status, payload)
	if regenerated["version"] != float64(5) || !strings.Contains(string(payload), sentinel) {
		t.Fatalf("generate control: %s", payload)
	}
	newConversation := uuid.MustParse(regenerated["generation"].(map[string]any)["conversation_id"].(string))
	if newConversation == conversationID {
		t.Fatal("generate did not replace conversation")
	}
	newStored, err := env.agentRepo.FindConversationByID(env.ctx, newConversation)
	if err != nil {
		t.Fatal(err)
	}
	inherited := false
	for _, ref := range newStored.ContextItems {
		inherited = inherited || ref.ID == sourceID || ref.ID == conversationID.String()
	}
	if !inherited {
		t.Fatal("replacement generation did not inherit trusted source dependencies")
	}

	readRoutes := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, path, nil},
		{http.MethodGet, f.base, nil},
		{http.MethodPost, path + "/preview", map[string]any{"expected_version": 5}},
	}
	for _, route := range readRoutes {
		status, payload = raw(route.method, route.path, route.body)
		if status != http.StatusOK || !strings.Contains(string(payload), sentinel) {
			t.Fatalf("allowed draft control %s %s: %d %s", route.method, route.path, status, payload)
		}
	}
	if _, err := env.agentSvc.AuthorizeRun(authz.WithActorID(env.ctx, owner.ID), conversationID, aid, false); err != nil {
		t.Fatalf("pre-revocation run authorization: %v", err)
	}
	before := openState("before revoke")
	status, payload = raw(http.MethodDelete, sourcePath, nil)
	if status != http.StatusNoContent {
		t.Fatalf("source delete: %d %s", status, payload)
	}
	after := openState("after revoke")
	oldEpoch, _ := strconv.ParseInt(before.Epoch, 10, 64)
	newEpoch, _ := strconv.ParseInt(after.Epoch, 10, 64)
	if newEpoch <= oldEpoch {
		t.Fatal("revocation did not advance authority epoch")
	}
	status, payload = raw(http.MethodGet, sourcePath, nil)
	if status != http.StatusNotFound || strings.Contains(string(payload), sentinel) {
		t.Fatalf("source still readable: %d %s", status, payload)
	}
	if _, err := env.agentSvc.AuthorizeRun(env.ctx, conversationID, aid, false); !agentsvc.RunDenied(err) {
		t.Fatalf("old run was not denied: %v", err)
	}
	if _, err := env.agentSvc.AuthorizeRun(env.ctx, newConversation, aid, false); !agentsvc.RunDenied(err) {
		t.Fatalf("replacement run did not retain revoked dependency: %v", err)
	}

	for _, route := range readRoutes {
		status, payload = raw(route.method, route.path, route.body)
		if status >= 500 {
			t.Errorf("%s %s availability failure: %d %s", route.method, route.path, status, payload)
		}
		if strings.Contains(string(payload), sentinel) {
			t.Errorf("revoked derived body leaked from %s %s: %d %s", route.method, route.path, status, payload)
		}
	}

	// Every endpoint that can return the draft body must recheck the same
	// server provenance. A revoked source cannot be bypassed through a write
	// response, a failed generation/import response, or an idempotent publish.
	writeRoutes := []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, path, map[string]any{
			"expected_version": 5,
			"goal":             "REVOKED_UPDATE",
			"items":            content["items"],
			"sources":          []string{},
			"assumptions":      []string{},
		}},
		{http.MethodPost, path + "/generate", map[string]any{"expected_version": 5, "agent_id": aid}},
		{http.MethodPost, path + "/import", map[string]any{"expected_version": 5, "generation_id": generation["id"]}},
		{http.MethodPost, path + "/publish", map[string]any{"expected_version": 5, "publication_id": uuid.NewString(), "confirmed": true}},
	}
	for _, route := range writeRoutes {
		status, payload = raw(route.method, route.path, route.body)
		if status >= 500 {
			t.Errorf("%s %s availability failure: %d %s", route.method, route.path, status, payload)
		}
		if status == http.StatusOK || status == http.StatusCreated || strings.Contains(string(payload), sentinel) {
			t.Errorf("revoked derived body delivered from %s %s: %d %s", route.method, route.path, status, payload)
		}
	}
}

func TestTask4bLegacyDraftWithoutProvenanceFailsClosed(t *testing.T) {
	f := newDraftFixture(t)
	draft := f.request(t, http.MethodPost, f.base, f.content(), http.StatusCreated)
	draftID := draft["id"].(string)
	// Simulate a pre-provenance row. No client-controlled display field can
	// upgrade this row to trusted source history.
	if _, err := f.env.db.Exec("UPDATE task_drafts SET content = content - '_provenance' WHERE id = $1", draftID); err != nil {
		t.Fatal(err)
	}
	path := f.base + "/" + draftID
	assertDenied := func(method, route string, body any) {
		t.Helper()
		req := mustRequest(f.env.ctx, t, method, f.env.base+route, nil)
		if body != nil {
			req = mustRequest(f.env.ctx, t, method, f.env.base+route, jsonBody(t, body))
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+f.token)
		resp := mustDo(t, f.client, req)
		defer resp.Body.Close()
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
			t.Fatalf("legacy draft was delivered by %s %s: %d %s", method, route, resp.StatusCode, payload)
		}
		if strings.Contains(string(payload), "PRIVATE_") {
			t.Fatalf("legacy draft body leaked from %s %s: %d %s", method, route, resp.StatusCode, payload)
		}
	}
	assertDenied(http.MethodGet, path, nil)
	// List filters unknown provenance rows instead of returning their bodies.
	listReq := mustRequest(f.env.ctx, t, http.MethodGet, f.env.base+f.base, nil)
	listReq.Header.Set("Authorization", "Bearer "+f.token)
	listResp := mustDo(t, f.client, listReq)
	defer listResp.Body.Close()
	listPayload, err := io.ReadAll(listResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if listResp.StatusCode != http.StatusOK || strings.Contains(string(listPayload), "PRIVATE_") {
		t.Fatalf("legacy draft list leaked or failed: %d %s", listResp.StatusCode, listPayload)
	}
	var listEnvelope envelope
	if err := json.Unmarshal(listPayload, &listEnvelope); err != nil {
		t.Fatal(err)
	}
	items, ok := assertDataMap(t, listEnvelope)["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("legacy draft was not filtered: %s", listPayload)
	}
	assertDenied(http.MethodPost, path+"/preview", map[string]any{"expected_version": 1})
	assertDenied(http.MethodPatch, path, map[string]any{
		"expected_version": 1,
		"goal":             "PATCH_SHOULD_NOT_UPGRADE",
		"items":            f.content()["items"],
		"sources":          []string{},
		"assumptions":      []string{},
	})
	assertDenied(http.MethodPost, path+"/generate", map[string]any{"expected_version": 1, "agent_id": uuid.NewString()})
	assertDenied(http.MethodPost, path+"/import", map[string]any{"expected_version": 1, "generation_id": uuid.NewString()})
	assertDenied(http.MethodPost, path+"/publish", map[string]any{"expected_version": 1, "publication_id": uuid.NewString(), "confirmed": true})
}
