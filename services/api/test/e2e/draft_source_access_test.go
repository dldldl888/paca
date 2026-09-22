package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	"github.com/Paca-AI/api/internal/platform/authz"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	agentsvc "github.com/Paca-AI/api/internal/service/agent"
	docsvc "github.com/Paca-AI/api/internal/service/doc"
	"github.com/google/uuid"
)

type task4bImportedDraft struct {
	f          *draftFixture
	env        *e2eEnv
	aid        uuid.UUID
	sourceID   string
	sourcePath string
	draftID    string
	draftPath  string
	sentinel   string
}

type task4bDraftSnapshot struct {
	version       int
	stateText     string
	publicationID *uuid.UUID
	content       []byte
}

func task4bSnapshot(t *testing.T, env *e2eEnv, id string) task4bDraftSnapshot {
	t.Helper()
	var snapshot task4bDraftSnapshot
	err := env.db.QueryRow(`SELECT version, state, publication_id, content FROM task_drafts WHERE id=$1`, id).
		Scan(&snapshot.version, &snapshot.stateText, &snapshot.publicationID, &snapshot.content)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func task4bPersistenceCounts(t *testing.T, env *e2eEnv) []int64 {
	t.Helper()
	counts := make([]int64, 0, 5)
	for _, table := range []string{"task_drafts", "agent_conversations", "agent_chat_sessions", "agent_pending_triggers", "agent_conversation_events"} {
		var n int64
		if err := env.db.Get(&n, "SELECT count(*) FROM "+table); err != nil {
			t.Fatal(err)
		}
		counts = append(counts, n)
	}
	return counts
}

func task4bRaw(t *testing.T, f *draftFixture, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader *bytes.Buffer
	if body != nil {
		reader = jsonBody(t, body)
	}
	req := mustRequest(f.env.ctx, t, method, f.env.base+path, reader)
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

// setupTask4bImportedDraft uses the production HTTP source and Draft routes,
// then simulates only the Hermes completion events consumed by Import.
func setupTask4bImportedDraft(t *testing.T) *task4bImportedDraft {
	t.Helper()
	f := newDraftFixture(t)
	env := f.env
	aid := draftHermes(t, f)
	other := uuid.MustParse(createProjectForTasksViaAPI(t, env, f.client, f.token))
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

	sentinel := "TASK4B_DERIVED_" + uuid.NewString()
	docBase := "/api/v1/projects/" + other.String() + "/docs"
	folder := f.request(t, http.MethodPost, docBase+"/folders", map[string]any{"name": "公司资料"}, http.StatusCreated)
	source := f.request(t, http.MethodPost, docBase, map[string]any{
		"title": "source", "visibility": "project", "folder_id": folder["id"],
		"content": []any{map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": sentinel}}}},
	}, http.StatusCreated)
	sourceID := source["id"].(string)
	sourcePath := docBase + "/" + sourceID
	status, payload := task4bRaw(t, f, http.MethodGet, sourcePath, nil)
	if status != http.StatusOK || !strings.Contains(string(payload), sentinel) {
		t.Fatalf("source control: %d %s", status, payload)
	}

	draft := f.request(t, http.MethodPost, f.base, f.content(), http.StatusCreated)
	draftID := draft["id"].(string)
	draftPath := f.base + "/" + draftID
	generated := f.request(t, http.MethodPost, draftPath+"/generate", map[string]any{"expected_version": 1, "agent_id": aid}, http.StatusOK)
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
	imported := f.request(t, http.MethodPost, draftPath+"/import", map[string]any{"expected_version": 2, "generation_id": generation["id"]}, http.StatusOK)
	if imported["version"] != float64(3) {
		t.Fatalf("import version: %+v", imported)
	}
	return &task4bImportedDraft{f: f, env: env, aid: aid, sourceID: sourceID, sourcePath: sourcePath, draftID: draftID, draftPath: draftPath, sentinel: sentinel}
}

type task4bHTTPRoute struct {
	name, method, path string
	body               any
}

func task4bRoute(t *testing.T, fx *task4bImportedDraft, name string) task4bHTTPRoute {
	t.Helper()
	f := fx.f
	switch name {
	case "get":
		return task4bHTTPRoute{name: name, method: http.MethodGet, path: fx.draftPath}
	case "list":
		return task4bHTTPRoute{name: name, method: http.MethodGet, path: f.base}
	case "preview":
		return task4bHTTPRoute{name: name, method: http.MethodPost, path: fx.draftPath + "/preview", body: map[string]any{"expected_version": 3}}
	case "update":
		path, _ := addTask4bImportedDraft(t, fx)
		body := f.content()
		body["goal"] = fx.sentinel
		body["expected_version"] = 3
		return task4bHTTPRoute{name: name, method: http.MethodPatch, path: path, body: body}
	case "generate":
		path, _ := addTask4bImportedDraft(t, fx)
		return task4bHTTPRoute{name: name, method: http.MethodPost, path: path + "/generate", body: map[string]any{"expected_version": 3, "agent_id": fx.aid}}
	case "import":
		path, generation := addTask4bGeneratedDraft(t, fx)
		completeTask4bGeneration(t, f, uuid.MustParse(generation["conversation_id"].(string)), fx.sentinel, fx.sourceID)
		return task4bHTTPRoute{name: name, method: http.MethodPost, path: path + "/import", body: map[string]any{"expected_version": 2, "generation_id": generation["id"]}}
	case "publish":
		path, _ := addTask4bImportedDraft(t, fx)
		return task4bHTTPRoute{name: name, method: http.MethodPost, path: path + "/publish", body: map[string]any{"expected_version": 3, "publication_id": uuid.New(), "confirmed": true}}
	default:
		t.Fatalf("unknown Task4b route %q", name)
		return task4bHTTPRoute{}
	}
}

func TestTask4bDraftHTTPDeliveryFlushesBeforeSourceRevocation(t *testing.T) {
	for _, name := range []string{"get", "list", "preview", "update", "generate", "import", "publish"} {
		t.Run(name, func(t *testing.T) {
			fx := setupTask4bImportedDraft(t)
			env, f := fx.env, fx.f
			route := task4bRoute(t, fx, name)
			headerEntered, headerRelease := make(chan int, 1), make(chan struct{})
			flushEntered, flushRelease := make(chan struct{}), make(chan struct{})
			var headerOnce, flushOnce sync.Once
			releaseHeader := func() { headerOnce.Do(func() { close(headerRelease) }) }
			releaseFlush := func() { flushOnce.Do(func() { close(flushRelease) }) }
			defer func() {
				releaseHeader()
				releaseFlush()
			}()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				env.handler.ServeHTTP(&heldTaskJSON{ResponseWriter: w, headerEntered: headerEntered, headerRelease: headerRelease, flushEntered: flushEntered, flushRelease: flushRelease}, r)
			}))
			defer server.Close()

			requestBody := (*bytes.Buffer)(nil)
			if route.body != nil {
				requestBody = jsonBody(t, route.body)
			}
			request := mustRequest(env.ctx, t, route.method, server.URL+route.path, requestBody)
			request.Header.Set("Authorization", "Bearer "+f.token)
			if route.body != nil {
				request.Header.Set("Content-Type", "application/json")
			}
			result := make(chan struct {
				status int
				body   string
				err    error
			}, 1)
			go func() {
				response, err := server.Client().Do(request)
				if err != nil {
					result <- struct {
						status int
						body   string
						err    error
					}{err: err}
					return
				}
				body, readErr := io.ReadAll(response.Body)
				response.Body.Close()
				result <- struct {
					status int
					body   string
					err    error
				}{status: response.StatusCode, body: string(body), err: readErr}
			}()

			select {
			case status := <-headerEntered:
				if status < http.StatusOK || status >= http.StatusMultipleChoices {
					t.Fatalf("authorized %s status=%d, want 2xx", name, status)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("%s response never reached WriteHeader", name)
			}

			deleteRequest := mustRequest(env.ctx, t, http.MethodDelete, env.base+fx.sourcePath, nil)
			deleteRequest.Header.Set("Authorization", "Bearer "+f.token)
			deleteResult := make(chan struct {
				status int
				body   string
				err    error
			}, 1)
			go func() {
				response, err := f.client.Do(deleteRequest)
				if err != nil {
					deleteResult <- struct {
						status int
						body   string
						err    error
					}{err: err}
					return
				}
				body, readErr := io.ReadAll(response.Body)
				response.Body.Close()
				deleteResult <- struct {
					status int
					body   string
					err    error
				}{status: response.StatusCode, body: string(body), err: readErr}
			}()
			deadline := time.After(3 * time.Second)
			for !env.fileGate.State().Blocked {
				select {
				case deleted := <-deleteResult:
					t.Fatalf("%s source revocation finished before delivery lease: %+v", name, deleted)
				case <-deadline:
					t.Fatalf("%s source revocation never entered coordinator barrier", name)
				case <-time.After(time.Millisecond):
				}
			}
			var active bool
			if err := env.db.Get(&active, "SELECT deleted_at IS NULL FROM documents WHERE id=$1", fx.sourceID); err != nil || !active {
				t.Fatalf("%s source changed before Flush: active=%v err=%v", name, active, err)
			}
			releaseHeader()
			select {
			case <-flushEntered:
			case deleted := <-deleteResult:
				t.Fatalf("%s source revocation finished before actual Flush: %+v", name, deleted)
			case <-time.After(3 * time.Second):
				t.Fatalf("%s response never reached actual Flush", name)
			}
			select {
			case deleted := <-deleteResult:
				t.Fatalf("%s source revocation finished before Flush release: %+v", name, deleted)
			default:
			}
			if err := env.db.Get(&active, "SELECT deleted_at IS NULL FROM documents WHERE id=$1", fx.sourceID); err != nil || !active {
				t.Fatalf("%s source changed before Flush release: active=%v err=%v", name, active, err)
			}
			releaseFlush()
			select {
			case delivered := <-result:
				if delivered.err != nil || delivered.status < http.StatusOK || delivered.status >= http.StatusMultipleChoices || !strings.Contains(delivered.body, fx.sentinel) {
					t.Fatalf("legal %s response not delivered: %+v", name, delivered)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("%s response did not finish after Flush release", name)
			}
			select {
			case deleted := <-deleteResult:
				if deleted.err != nil || deleted.status != http.StatusNoContent {
					t.Fatalf("%s source revocation failed after Flush: %+v", name, deleted)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("%s source revocation did not commit after Flush", name)
			}
			waitFileGate(t, env)
		})
	}
}

func addTask4bGeneratedDraft(t *testing.T, fx *task4bImportedDraft) (string, map[string]any) {
	t.Helper()
	f, env := fx.f, fx.env
	draft := f.request(t, http.MethodPost, f.base, f.content(), http.StatusCreated)
	path := f.base + "/" + draft["id"].(string)
	generated := f.request(t, http.MethodPost, path+"/generate", map[string]any{"expected_version": 1, "agent_id": fx.aid}, http.StatusOK)
	generation := generated["generation"].(map[string]any)
	cid := uuid.MustParse(generation["conversation_id"].(string))
	company, err := env.agentSvc.CompanyContext(env.ctx, cid, fx.aid)
	if err != nil || !strings.Contains(company.Text, fx.sentinel) {
		t.Fatalf("company context: %+v %v", company, err)
	}
	stored, err := env.agentRepo.FindConversationByID(env.ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ref := range stored.ContextItems {
		found = found || ref.ID == fx.sourceID
	}
	if !found {
		t.Fatal("source context was not persisted")
	}
	return path, generation
}

func addTask4bImportedDraft(t *testing.T, fx *task4bImportedDraft) (string, uuid.UUID) {
	t.Helper()
	f := fx.f
	path, generation := addTask4bGeneratedDraft(t, fx)
	completeTask4bGeneration(t, f, uuid.MustParse(generation["conversation_id"].(string)), fx.sentinel, fx.sourceID)
	f.request(t, http.MethodPost, path+"/import", map[string]any{"expected_version": 2, "generation_id": generation["id"]}, http.StatusOK)
	return path, uuid.MustParse(generation["id"].(string))
}

func completeTask4bGeneration(t *testing.T, f *draftFixture, cid uuid.UUID, sentinel, sourceID string) {
	t.Helper()
	c := f.content()
	for _, item := range c["items"].([]any) {
		row := item.(map[string]any)
		row["title"] = sentinel
		row["requirements"] = "derived from " + sentinel
	}
	model, err := json.Marshal(map[string]any{"items": c["items"], "sources": []string{sourceID}, "assumptions": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range []struct {
		kind, source string
		payload      map[string]any
	}{
		{"user_message", "user", map[string]any{"content": map[string]any{"type": "text", "text": "synthetic task4b input"}}},
		{"agent_message_chunk", "agent", map[string]any{"content": map[string]any{"type": "text", "text": string(model)}}},
		{"turn_end", "system", map[string]any{"stopReason": "end_turn"}},
	} {
		if err := f.env.agentRepo.CreateConversationEvent(f.env.ctx, &agentdom.AgentConversationEvent{ID: uuid.New(), ConversationID: cid, EventIndex: i, EventType: ev.kind, EventSource: ev.source, Payload: ev.payload, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.env.agentRepo.UpdateConversationStatus(f.env.ctx, cid, "finished"); err != nil {
		t.Fatal(err)
	}
}

func TestTask4bPublishReplayAndRevokedWriteHaveNoSideEffects(t *testing.T) {
	fx := setupTask4bImportedDraft(t)
	f, env := fx.f, fx.env
	publication := uuid.New()
	beforePublish := f.counts(t)
	status, payload := task4bRaw(t, f, http.MethodPost, fx.draftPath+"/publish", map[string]any{"expected_version": 3, "publication_id": publication, "confirmed": true})
	if status != http.StatusOK || !strings.Contains(string(payload), fx.sentinel) {
		t.Fatalf("legal publish control failed: %d %s", status, payload)
	}
	afterPublish := f.counts(t)
	if len(afterPublish) != len(beforePublish) || afterPublish[0] <= beforePublish[0] || afterPublish[4] <= beforePublish[4] {
		t.Fatalf("legal publish did not create durable task/notification rows: before=%v after=%v", beforePublish, afterPublish)
	}

	confirmPath, _ := addTask4bImportedDraft(t, fx)
	importPath, generationID := addTask4bImportedDraft(t, fx)
	generated := f.request(t, http.MethodPost, importPath+"/generate", map[string]any{"expected_version": 3, "agent_id": fx.aid}, http.StatusOK)
	currentGeneration := generated["generation"].(map[string]any)
	currentGenerationID := uuid.MustParse(currentGeneration["id"].(string))
	if currentGenerationID == generationID {
		t.Fatal("generate did not replace current generation")
	}
	completeDraftGeneration(t, f, uuid.MustParse(currentGeneration["conversation_id"].(string)))
	beforeRevoke := f.counts(t)
	beforePersistence := task4bPersistenceCounts(t, env)
	draftIDs := []string{fx.draftID, strings.TrimPrefix(confirmPath, f.base+"/"), strings.TrimPrefix(importPath, f.base+"/")}
	beforeDrafts := make(map[string]task4bDraftSnapshot, len(draftIDs))
	for _, id := range draftIDs {
		beforeDrafts[id] = task4bSnapshot(t, env, id)
	}
	status, payload = task4bRaw(t, f, http.MethodDelete, fx.sourcePath, nil)
	if status != http.StatusNoContent {
		t.Fatalf("source delete: %d %s", status, payload)
	}
	waitFileGate(t, env)

	// The same publication UUID and version must not enter Repo.Publish's
	// idempotent replay branch after source revocation.
	status, payload = task4bRaw(t, f, http.MethodPost, fx.draftPath+"/publish", map[string]any{"expected_version": 3, "publication_id": publication, "confirmed": true})
	if status != http.StatusNotFound || strings.Contains(string(payload), fx.sentinel) {
		t.Fatalf("revoked publish replay delivered body: %d %s", status, payload)
	}
	// Confirmation was supplied before revocation, but a first publish must
	// also fail before creating any formal rows.
	status, payload = task4bRaw(t, f, http.MethodPost, confirmPath+"/publish", map[string]any{"expected_version": 3, "publication_id": uuid.New(), "confirmed": true})
	if status != http.StatusNotFound || strings.Contains(string(payload), fx.sentinel) {
		t.Fatalf("revoked first publish delivered body: %d %s", status, payload)
	}
	// This is a current, completed generation/version; rejection cannot rely
	// on the superseded generation conflict.
	status, payload = task4bRaw(t, f, http.MethodPost, importPath+"/import", map[string]any{"expected_version": 4, "generation_id": currentGenerationID})
	if status != http.StatusNotFound || strings.Contains(string(payload), fx.sentinel) {
		t.Fatalf("revoked current import delivered body: %d %s", status, payload)
	}
	updateBody := f.content()
	updateBody["goal"] = fx.sentinel
	updateBody["expected_version"] = 3
	status, payload = task4bRaw(t, f, http.MethodPatch, confirmPath, updateBody)
	if status != http.StatusNotFound || strings.Contains(string(payload), fx.sentinel) {
		t.Fatalf("revoked update delivered body: %d %s", status, payload)
	}
	status, payload = task4bRaw(t, f, http.MethodPost, confirmPath+"/generate", map[string]any{"expected_version": 3, "agent_id": fx.aid})
	if status != http.StatusNotFound || strings.Contains(string(payload), fx.sentinel) {
		t.Fatalf("revoked generate delivered body: %d %s", status, payload)
	}
	afterReject := f.counts(t)
	if !slices.Equal(beforeRevoke, afterReject) {
		t.Fatalf("revoked writes changed durable counts: before=%v after=%v", beforeRevoke, afterReject)
	}
	if after := task4bPersistenceCounts(t, env); !slices.Equal(beforePersistence, after) {
		t.Fatalf("revoked writes changed persistence: before=%v after=%v", beforePersistence, after)
	}
	for _, id := range draftIDs {
		before := beforeDrafts[id]
		after := task4bSnapshot(t, env, id)
		if before.version != after.version || before.stateText != after.stateText || before.publicationID == nil && after.publicationID != nil || before.publicationID != nil && after.publicationID == nil || before.publicationID != nil && after.publicationID != nil && *before.publicationID != *after.publicationID || !bytes.Equal(before.content, after.content) {
			t.Fatalf("revoked write changed draft %s: before=%+v after=%+v", id, before, after)
		}
	}
	if snapshot := beforeDrafts[fx.draftID]; snapshot.stateText != "published" || snapshot.publicationID == nil || *snapshot.publicationID != publication {
		t.Fatalf("legal publication was not retained before revoke: %+v", snapshot)
	}
}

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
	assertDenied := func(tb *testing.T, method, route string, body any) {
		tb.Helper()
		req := mustRequest(f.env.ctx, tb, method, f.env.base+route, nil)
		if body != nil {
			req = mustRequest(f.env.ctx, tb, method, f.env.base+route, jsonBody(tb, body))
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+f.token)
		resp := mustDo(tb, f.client, req)
		defer resp.Body.Close()
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			tb.Fatal(err)
		}
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			tb.Fatalf("legacy draft was delivered by %s %s: %d %s", method, route, resp.StatusCode, payload)
		}
		if strings.Contains(string(payload), "PRIVATE_") {
			tb.Fatalf("legacy draft body leaked from %s %s: %d %s", method, route, resp.StatusCode, payload)
		}
	}
	assertDenied(t, http.MethodGet, path, nil)
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
	assertDenied(t, http.MethodPost, path+"/preview", map[string]any{"expected_version": 1})
	assertDenied(t, http.MethodPatch, path, map[string]any{
		"expected_version": 1,
		"goal":             "PATCH_SHOULD_NOT_UPGRADE",
		"items":            f.content()["items"],
		"sources":          []string{},
		"assumptions":      []string{},
	})
	assertDenied(t, http.MethodPost, path+"/generate", map[string]any{"expected_version": 1, "agent_id": uuid.NewString()})
	assertDenied(t, http.MethodPost, path+"/import", map[string]any{"expected_version": 1, "generation_id": uuid.NewString()})
	assertDenied(t, http.MethodPost, path+"/publish", map[string]any{"expected_version": 1, "publication_id": uuid.NewString(), "confirmed": true})

	// Other legacy shapes must also fail closed without rewriting the stored
	// bytes. These are database states, not client payloads, so each case is
	// seeded directly and exercised through the production GET route.
	invalid := []struct {
		name        string
		provenance  any
		removeField bool
	}{
		{"unknown-version", map[string]any{"version": 99, "bindings": []any{}}, false},
		{"nil-bindings", map[string]any{"version": 1, "bindings": nil}, false},
		{"invalid-binding", map[string]any{"version": 1, "bindings": []any{map[string]any{"conversation_id": uuid.NewString(), "agent_id": uuid.NewString(), "chat_session_id": uuid.NewString()}}}, false},
		{"missing-provenance", nil, true},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			d := f.request(t, http.MethodPost, f.base, f.content(), http.StatusCreated)
			id := d["id"].(string)
			var stored map[string]any
			var original []byte
			if err := f.env.db.QueryRow("SELECT content FROM task_drafts WHERE id=$1", id).Scan(&original); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(original, &stored); err != nil {
				t.Fatal(err)
			}
			if tc.removeField {
				delete(stored, "_provenance")
			} else {
				stored["_provenance"] = tc.provenance
			}
			mutated, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.env.db.Exec("UPDATE task_drafts SET content=$2 WHERE id=$1", id, mutated); err != nil {
				t.Fatal(err)
			}
			var before []byte
			if err := f.env.db.QueryRow("SELECT content FROM task_drafts WHERE id=$1", id).Scan(&before); err != nil {
				t.Fatal(err)
			}
			assertDenied(t, http.MethodGet, f.base+"/"+id, nil)
			var after []byte
			if err := f.env.db.QueryRow("SELECT content FROM task_drafts WHERE id=$1", id).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("rejected legacy row was rewritten: before=%s after=%s", before, after)
			}
		})
	}
}
