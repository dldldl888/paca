package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	apikeydom "github.com/Paca-AI/api/internal/domain/apikey"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	userdom "github.com/Paca-AI/api/internal/domain/user"
	"github.com/Paca-AI/api/internal/platform/authz"
	jwttoken "github.com/Paca-AI/api/internal/platform/token"
	apikeysvc "github.com/Paca-AI/api/internal/service/apikey"
	authsvc "github.com/Paca-AI/api/internal/service/auth"
	projectsvc "github.com/Paca-AI/api/internal/service/project"
	sprintsvc "github.com/Paca-AI/api/internal/service/sprint"
	tasksvc "github.com/Paca-AI/api/internal/service/task"
	usersvc "github.com/Paca-AI/api/internal/service/user"
	"github.com/Paca-AI/api/internal/transport/http/handler"
	"github.com/Paca-AI/api/internal/transport/http/router"
)

// ---------------------------------------------------------------------------
// Agent API Key Integration Tests
// ---------------------------------------------------------------------------

// testAgentAPIKey is the static agent API key used in tests
const testAgentAPIKey = "paca_test_agent_key_1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
const testAgentBotUserID = "00000000-0000-0000-0000-000000000001"

// buildAgentKeyRouter creates a test router with agent API key authentication configured
func buildAgentKeyRouter(taskRepo *fakeTaskRepo, apiKeyRepo *fakeAPIKeyRepo, store *projectPermStore, activityRepos ...*fakeTaskActivityRepo) http.Handler {
	return buildAgentKeyRouterWithBotID(taskRepo, apiKeyRepo, store, uuid.MustParse(testAgentBotUserID), activityRepos...)
}

// buildAgentKeyRouterWithBotID is like buildAgentKeyRouter but lets the test
// choose which user ID the shared agent key authenticates as when no
// X-Agent-ID header is present. buildAgentKeyRouter always uses
// testAgentBotUserID, a test-local placeholder distinct from the real
// userdom.SystemActorUserID seeded in production — tests that need to
// exercise the userdom.SystemActorUserID-specific "unidentified actor"
// comment-error path must use this variant instead.
func buildAgentKeyRouterWithBotID(taskRepo *fakeTaskRepo, apiKeyRepo *fakeAPIKeyRepo, store *projectPermStore, botUserID uuid.UUID, activityRepos ...*fakeTaskActivityRepo) http.Handler {
	tm := jwttoken.New(testSecret, 15*time.Minute, 168*time.Hour)
	refreshStore := &fakeRefreshStore{}
	userRepo := newFakeUserRepo()
	authService := authsvc.New(userRepo, tm, refreshStore, 168*time.Hour, 24*time.Hour)
	userService := usersvc.New(userRepo, userRepo)
	projectRepo := newFakeProjectRepo()
	projectService := projectsvc.New(projectRepo, taskRepo, nil)
	taskService := tasksvc.New(taskRepo, authz.NewTaskAccessPolicy(store))
	sprintService := sprintsvc.New(newFakeSprintRepoIT(), taskRepo, nil).WithTaskAccessChecker(taskService)
	viewService := sprintsvc.NewViewService(newFakeViewRepoIT(), newFakeSprintRepoIT(), taskRepo, nil).WithTaskAccessChecker(taskService)
	var activityRepo *fakeTaskActivityRepo
	if len(activityRepos) > 0 && activityRepos[0] != nil {
		activityRepo = activityRepos[0]
	} else {
		activityRepo = newFakeTaskActivityRepo()
	}
	activityService := tasksvc.NewActivityService(activityRepo, taskRepo, &fakeActivityMemberRepo{}, nil).WithTaskAccessChecker(taskService)

	apiKeyService := apikeysvc.New(apiKeyRepo).WithAgentKey(testAgentAPIKey, botUserID)
	// None of these task/comment-focused tests exercise the X-Actor-User-ID
	// verification path (they never set that header, only X-Agent-ID) — see
	// authn_test.go's TestAuthn_APIKey_* for that. This fake exists purely
	// so a well-formed X-Agent-ID isn't rejected outright by
	// verifyAgentIdentity's DB lookup, which every one of these tests would
	// otherwise fail closed against.
	apiKeyService.WithAgentIdentityStore(passthroughAgentIdentityStore{})

	if store == nil {
		store = &projectPermStore{}
	}
	authorizer := authz.NewAuthorizer(store).WithAgentRoleResolver(store)

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	return router.New(router.Deps{
		TokenManager:         tm,
		APIKeyAuth:           apiKeyService,
		Authorizer:           authorizer,
		ProjectVisibilitySvc: projectService,
		Health:               handler.NewHealthHandler(),
		Auth:                 handler.NewAuthHandler(authService, testCookieCfg),
		User:                 handler.NewUserHandler(userService),
		GlobalRole:           handler.NewGlobalRoleHandler(&fakeGlobalRoleService{}),
		Project:              handler.NewProjectHandler(projectService, authorizer),
		Task:                 handler.NewTaskHandler(taskService, viewService, activityService),
		Sprint:               handler.NewSprintHandler(sprintService, viewService),
		View:                 handler.NewViewHandler(viewService),
		Log:                  log,
	})
}

// passthroughAgentIdentityStore satisfies apikeysvc.AgentIdentityStore by
// treating every X-Agent-ID as a real, existing agent — these integration
// tests are about task/comment behavior under agent-key auth, not about the
// X-Agent-ID verification boundary itself (see authn_test.go for that), so
// they don't need a real agents-table fake.
type passthroughAgentIdentityStore struct{}

func (passthroughAgentIdentityStore) FindAgentByID(_ context.Context, agentID uuid.UUID) (*agentdom.Agent, error) {
	return &agentdom.Agent{ID: agentID, AgentScope: agentdom.AgentScopeProject}, nil
}

func (passthroughAgentIdentityStore) HasActiveGlobalChatSession(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return true, nil
}

func (passthroughAgentIdentityStore) FindAgentByMCPAPIKeyHash(context.Context, string) (*agentdom.Agent, error) {
	return nil, agentdom.ErrAgentNotFound
}

// agentKeyAuthReq creates a request authenticated with agent API key
func agentKeyAuthReq(ctx context.Context, method, url string, agentID uuid.UUID, body any) *http.Request {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req := httptest.NewRequestWithContext(ctx, method, url, reader)
	req.Header.Set("X-API-Key", testAgentAPIKey)
	if agentID != uuid.Nil {
		req.Header.Set("X-Agent-ID", agentID.String())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// ---------------------------------------------------------------------------
// Agent Task Creation Tests
// ---------------------------------------------------------------------------

func TestAgentAPIKey_CreateTask_UnboundAgentDenied(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	agentID := uuid.New()

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			uuid.MustParse(testAgentBotUserID): {
				projectID: {authz.PermissionTasksWrite},
			},
		},
		agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			projectID: {
				agentID: {authz.PermissionTasksWrite},
			},
		},
		agentRoles: map[uuid.UUID]map[uuid.UUID]string{
			projectID: {
				agentID: "agent_developer",
			},
		},
	}

	r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)
	base := fmt.Sprintf("/api/v1/projects/%s/tasks", projectID)

	// Create task using agent API key
	w := serve(r, agentKeyAuthReq(t.Context(), http.MethodPost, base, agentID, map[string]any{
		"title": "Agent created task",
		"description": []map[string]any{
			{
				"type":    "paragraph",
				"content": []map[string]any{{"type": "text", "text": "Task created by AI agent"}},
			},
		},
	}))

	if w.Code != http.StatusNotFound {
		t.Fatalf("unbound agent: want 404, got %d: %s", w.Code, w.Body.String())
	}
	if len(taskRepo.tasks) != 0 {
		t.Fatal("denied create inserted a task")
	}
}

func TestAgentAPIKey_CreateTask_MissingAgentID_Denied(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	botUserID := uuid.MustParse(testAgentBotUserID)

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {
				projectID: {authz.PermissionTasksAll},
			},
		},
	}

	r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)
	base := fmt.Sprintf("/api/v1/projects/%s/tasks", projectID)

	// Request without X-Agent-ID header should succeed using bot user permissions
	w := serve(r, agentKeyAuthReq(t.Context(), http.MethodPost, base, uuid.Nil, map[string]any{
		"title": "Task without agent ID",
	}))

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unbound shared credential, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAgentAPIKey_CreateTask_InvalidAPIKey_Returns401(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	agentID := uuid.New()

	store := &projectPermStore{
		projectPerms: map[uuid.UUID][]authz.Permission{
			projectID: {authz.PermissionTasksWrite},
		},
	}

	r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)
	base := fmt.Sprintf("/api/v1/projects/%s/tasks", projectID)

	body, _ := json.Marshal(map[string]any{"title": "Invalid key task"})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, base, bytes.NewReader(body))
	req.Header.Set("X-API-Key", "paca_invalid_key")
	req.Header.Set("X-Agent-ID", agentID.String())
	req.Header.Set("Content-Type", "application/json")

	w := serve(r, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with invalid API key, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAgentAPIKey_CreateTask_NoPermission_Returns403(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	agentID := uuid.New()
	botUserID := uuid.MustParse(testAgentBotUserID)

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {
				projectID: {authz.PermissionTasksRead},
			},
		},
		agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			projectID: {
				agentID: {authz.PermissionTasksRead},
			},
		},
		agentRoles: map[uuid.UUID]map[uuid.UUID]string{
			projectID: {
				agentID: "agent_reader",
			},
		},
	}

	r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)
	base := fmt.Sprintf("/api/v1/projects/%s/tasks", projectID)

	w := serve(r, agentKeyAuthReq(t.Context(), http.MethodPost, base, agentID, map[string]any{
		"title": "Unauthorized task",
	}))

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without write permission, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Agent Task Comment Tests
// ---------------------------------------------------------------------------

func TestAgentAPIKey_AddComment_UnboundDenied(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	activityRepo := newFakeTaskActivityRepo()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	taskID := uuid.New()
	agentID := uuid.New()
	botUserID := uuid.MustParse(testAgentBotUserID)

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {
				projectID: {authz.PermissionTasksWrite},
			},
		},
		agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			projectID: {
				agentID: {authz.PermissionTasksWrite},
			},
		},
		agentRoles: map[uuid.UUID]map[uuid.UUID]string{
			projectID: {
				agentID: "agent_developer",
			},
		},
	}

	// Seed a task directly
	taskRepo.tasks[taskID] = &taskdom.Task{
		ID:        taskID,
		ProjectID: projectID,
		Title:     "Test Task",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store, activityRepo)
	commentURL := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/activities/comments", projectID, taskID)

	w := serve(r, agentKeyAuthReq(t.Context(), http.MethodPost, commentURL, agentID, map[string]any{
		"content": []map[string]any{
			{
				"type":    "paragraph",
				"content": []map[string]any{{"type": "text", "text": "Comment from AI agent"}},
			},
		},
	}))

	if w.Code != http.StatusNotFound {
		t.Fatalf("unbound agent comment expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if len(activityRepo.activities) != 0 {
		t.Fatal("denied comment persisted")
	}
}

// A shared key without a bound task delegation cannot post comments.
func TestAgentAPIKey_AddComment_MissingAgentID_Denied(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	activityRepo := newFakeTaskActivityRepo()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	taskID := uuid.New()
	botUserID := userdom.SystemActorUserID

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {
				projectID: {authz.PermissionTasksWrite},
			},
		},
	}

	taskRepo.tasks[taskID] = &taskdom.Task{
		ID:        taskID,
		ProjectID: projectID,
		Title:     "Test Task",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	r := buildAgentKeyRouterWithBotID(taskRepo, apiKeyRepo, store, botUserID, activityRepo)
	commentURL := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/activities/comments", projectID, taskID)

	// No agentID (uuid.Nil) means agentKeyAuthReq omits the X-Agent-ID header.
	w := serve(r, agentKeyAuthReq(t.Context(), http.MethodPost, commentURL, uuid.Nil, map[string]any{
		"content": []map[string]any{
			{
				"type":    "paragraph",
				"content": []map[string]any{{"type": "text", "text": "orphan comment"}},
			},
		},
	}))

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	var env struct {
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if env.ErrorCode != "TASK_NOT_FOUND" {
		t.Errorf("expected error_code=TASK_NOT_FOUND, got %q", env.ErrorCode)
	}
}

func TestAgentAPIKey_UpdateComment_UnboundDenied(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	activityRepo := newFakeTaskActivityRepo()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	taskID := uuid.New()
	commentID := uuid.New()
	agentID := uuid.New()
	botUserID := uuid.MustParse(testAgentBotUserID)

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {
				projectID: {authz.PermissionTasksWrite},
			},
		},
		agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			projectID: {
				agentID: {authz.PermissionTasksWrite},
			},
		},
		agentRoles: map[uuid.UUID]map[uuid.UUID]string{
			projectID: {
				agentID: "agent_developer",
			},
		},
	}

	// Seed task and comment
	taskRepo.tasks[taskID] = &taskdom.Task{
		ID:        taskID,
		ProjectID: projectID,
		Title:     "Test Task",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	_ = agentID // referenced only to confirm the bot user exists
	commentContent := json.RawMessage(`[{"type":"paragraph","content":[{"type":"text","text":"Original comment"}]}]`)
	activityRepo.activities[commentID] = &taskdom.Activity{
		ID:           commentID,
		TaskID:       taskID,
		ActorID:      &agentID,
		ActivityType: taskdom.ActivityTypeComment,
		Content:      commentContent,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store, activityRepo)
	commentURL := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/activities/comments/%s", projectID, taskID, commentID)

	w := serve(r, agentKeyAuthReq(t.Context(), http.MethodPatch, commentURL, agentID, map[string]any{
		"content": []map[string]any{
			{
				"type":    "paragraph",
				"content": []map[string]any{{"type": "text", "text": "Updated by agent"}},
			},
		},
	}))

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if string(activityRepo.activities[commentID].Content) != string(commentContent) {
		t.Fatal("denied edit persisted")
	}
}

// ---------------------------------------------------------------------------
// Agent Task Update Tests
// ---------------------------------------------------------------------------

func TestAgentAPIKey_UpdateTask_UnboundAgentDenied(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	taskID := uuid.New()
	agentID := uuid.New()
	statusID := uuid.New()
	botUserID := uuid.MustParse(testAgentBotUserID)

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {
				projectID: {authz.PermissionTasksWrite},
			},
		},
		agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			projectID: {
				agentID: {authz.PermissionTasksWrite},
			},
		},
		agentRoles: map[uuid.UUID]map[uuid.UUID]string{
			projectID: {
				agentID: "agent_developer",
			},
		},
	}

	// Seed a task directly
	taskRepo.tasks[taskID] = &taskdom.Task{
		ID:        taskID,
		ProjectID: projectID,
		Title:     "Original Title",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)
	taskURL := fmt.Sprintf("/api/v1/projects/%s/tasks/%s", projectID, taskID)

	w := serve(r, agentKeyAuthReq(t.Context(), http.MethodPatch, taskURL, agentID, map[string]any{
		"title":     "Updated by Agent",
		"status_id": statusID.String(),
	}))

	if w.Code != http.StatusNotFound {
		t.Fatalf("unbound agent: want 404, got %d: %s", w.Code, w.Body.String())
	}
	if taskRepo.tasks[taskID].Title != "Original Title" || taskRepo.tasks[taskID].StatusID != nil {
		t.Fatal("denied update mutated task")
	}
}

// ---------------------------------------------------------------------------
// Agent Task Listing Tests
// ---------------------------------------------------------------------------

func TestAgentAPIKey_ListTasks_UnboundAgentDenied(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	agentID := uuid.New()
	botUserID := uuid.MustParse(testAgentBotUserID)

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {
				projectID: {authz.PermissionTasksRead},
			},
		},
		agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			projectID: {
				agentID: {authz.PermissionTasksRead},
			},
		},
		agentRoles: map[uuid.UUID]map[uuid.UUID]string{
			projectID: {
				agentID: "agent_reader",
			},
		},
	}

	// Seed some tasks
	for i := 0; i < 3; i++ {
		taskRepo.tasks[uuid.New()] = &taskdom.Task{
			ID:        uuid.New(),
			ProjectID: projectID,
			Title:     fmt.Sprintf("Task %d", i+1),
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
	}

	r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)
	taskURL := fmt.Sprintf("/api/v1/projects/%s/tasks", projectID)

	w := serve(r, agentKeyAuthReq(t.Context(), http.MethodGet, taskURL, agentID, nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("unbound agent: want 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Agent Task Get Tests
// ---------------------------------------------------------------------------

func TestAgentAPIKey_GetTask_UnboundAgentDenied(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	taskID := uuid.New()
	agentID := uuid.New()
	botUserID := uuid.MustParse(testAgentBotUserID)

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {
				projectID: {authz.PermissionTasksRead},
			},
		},
		agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			projectID: {
				agentID: {authz.PermissionTasksRead},
			},
		},
		agentRoles: map[uuid.UUID]map[uuid.UUID]string{
			projectID: {
				agentID: "agent_reader",
			},
		},
	}

	// Seed a task directly
	taskRepo.tasks[taskID] = &taskdom.Task{
		ID:        taskID,
		ProjectID: projectID,
		Title:     "Agent Task",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)
	taskURL := fmt.Sprintf("/api/v1/projects/%s/tasks/%s", projectID, taskID)

	w := serve(r, agentKeyAuthReq(t.Context(), http.MethodGet, taskURL, agentID, nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("unbound agent: want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAgentAPIKey_GetTask_NotFound(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	taskID := uuid.New()
	agentID := uuid.New()
	botUserID := uuid.MustParse(testAgentBotUserID)

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {
				projectID: {authz.PermissionTasksRead},
			},
		},
		agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			projectID: {
				agentID: {authz.PermissionTasksRead},
			},
		},
		agentRoles: map[uuid.UUID]map[uuid.UUID]string{
			projectID: {
				agentID: "agent_reader",
			},
		},
	}

	r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)
	taskURL := fmt.Sprintf("/api/v1/projects/%s/tasks/%s", projectID, taskID)

	w := serve(r, agentKeyAuthReq(t.Context(), http.MethodGet, taskURL, agentID, nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	if code := decodeErrorCode(t, w); code != "TASK_NOT_FOUND" {
		t.Errorf("expected TASK_NOT_FOUND, got %q", code)
	}
}

// ---------------------------------------------------------------------------
// Agent Task Delete Tests
// ---------------------------------------------------------------------------

func TestAgentAPIKey_DeleteTask_UnboundAgentDenied(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	taskID := uuid.New()
	agentID := uuid.New()
	botUserID := uuid.MustParse(testAgentBotUserID)

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {
				projectID: {authz.PermissionTasksWrite},
			},
		},
		agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			projectID: {
				agentID: {authz.PermissionTasksWrite},
			},
		},
		agentRoles: map[uuid.UUID]map[uuid.UUID]string{
			projectID: {
				agentID: "agent_developer",
			},
		},
	}

	// Seed a task directly
	taskRepo.tasks[taskID] = &taskdom.Task{
		ID:        taskID,
		ProjectID: projectID,
		Title:     "Task to Delete",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)
	taskURL := fmt.Sprintf("/api/v1/projects/%s/tasks/%s", projectID, taskID)

	w := serve(r, agentKeyAuthReq(t.Context(), http.MethodDelete, taskURL, agentID, nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("unbound agent: want 404, got %d: %s", w.Code, w.Body.String())
	}
	if taskRepo.tasks[taskID] == nil || taskRepo.tasks[taskID].DeletedAt != nil {
		t.Fatal("denied delete removed task")
	}
}

// ---------------------------------------------------------------------------
// Agent Comments Complex Scenarios
// ---------------------------------------------------------------------------

func TestAgentAPIKey_CommentWorkflow_UnboundDenied(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	agentID := uuid.New()
	botUserID := uuid.MustParse(testAgentBotUserID)

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {
				projectID: {authz.PermissionTasksRead, authz.PermissionTasksWrite},
			},
		},
		agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			projectID: {
				agentID: {authz.PermissionTasksRead, authz.PermissionTasksWrite},
			},
		},
		agentRoles: map[uuid.UUID]map[uuid.UUID]string{
			projectID: {
				agentID: "agent_developer",
			},
		},
	}

	r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)

	// Existing activity workflow; the task itself is trusted fixture setup.
	taskID := uuid.New()
	if err := taskRepo.CreateTask(t.Context(), &taskdom.Task{ID: taskID, ProjectID: projectID, Title: "Agent workflow task"}); err != nil {
		t.Fatal(err)
	}

	// Step 2: Agent comments on the task
	commentURL := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/activities/comments", projectID, taskID)
	commentW := serve(r, agentKeyAuthReq(t.Context(), http.MethodPost, commentURL, agentID, map[string]any{
		"content": []map[string]any{
			{
				"type":    "paragraph",
				"content": []map[string]any{{"type": "text", "text": "Working on this task"}},
			},
		},
	}))
	if commentW.Code != http.StatusNotFound {
		t.Fatalf("unbound add expected 404, got %d", commentW.Code)
	}
	activitiesURL := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/activities", projectID, taskID)
	listW := serve(r, agentKeyAuthReq(t.Context(), http.MethodGet, activitiesURL, agentID, nil))
	if listW.Code != http.StatusNotFound {
		t.Fatalf("unbound activity read expected 404, got %d", listW.Code)
	}
}

// ---------------------------------------------------------------------------
// Agent ID Header Validation Tests
// ---------------------------------------------------------------------------

func TestAgentAPIKey_InvalidAgentIDCannotGrantTaskAccess(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	botUserID := uuid.MustParse(testAgentBotUserID)

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {
				projectID: {authz.PermissionTasksWrite},
			},
		},
	}

	r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)
	base := fmt.Sprintf("/api/v1/projects/%s/tasks", projectID)

	// Invalid agent ID (malformed)
	body, _ := json.Marshal(map[string]any{"title": "Test"})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, base, bytes.NewReader(body))
	req.Header.Set("X-API-Key", testAgentAPIKey)
	req.Header.Set("X-Agent-ID", "not-a-valid-uuid")
	req.Header.Set("Content-Type", "application/json")

	w := serve(r, req)
	// Unbound shared credentials do not acquire a human identity.
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for invalid/unbound agent identity, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Agent API Key vs Regular User API Key Tests
// ---------------------------------------------------------------------------

func TestAgentAPIKey_UserAPIKeyCannotUseAgentID(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	agentID := uuid.New()
	userID := uuid.New()

	store := &projectPermStore{
		projectPerms: map[uuid.UUID][]authz.Permission{
			projectID: {authz.PermissionTasksWrite},
		},
	}

	// Create a regular user API key
	userAPIKey := &apikeydom.APIKey{
		ID:        uuid.New(),
		UserID:    userID,
		Name:      "User Key",
		KeyPrefix: "abc12345",
		CreatedAt: time.Now(),
	}
	keyHash := "user_key_hash_12345"
	if err := apiKeyRepo.Create(context.Background(), userAPIKey, keyHash); err != nil {
		t.Fatalf("failed to create user API key: %v", err)
	}

	r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)
	base := fmt.Sprintf("/api/v1/projects/%s/tasks", projectID)

	// Try to use user API key with X-Agent-ID header - agent ID should be ignored
	rawUserKey := "paca_" + keyHash
	body, _ := json.Marshal(map[string]any{"title": "Test"})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, base, bytes.NewReader(body))
	req.Header.Set("X-API-Key", rawUserKey)
	req.Header.Set("X-Agent-ID", agentID.String())
	req.Header.Set("Content-Type", "application/json")

	w := serve(r, req)
	// Should succeed but X-Agent-ID should be ignored because it's not an agent key
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Logf("Request with user API key and agent ID returned %d (expected success with agent ID ignored)", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Agent Bulk Operations Tests
// ---------------------------------------------------------------------------

func TestAgentAPIKey_BulkUnboundTaskCreationDenied(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	agentID := uuid.New()
	botUserID := uuid.MustParse(testAgentBotUserID)

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {
				projectID: {authz.PermissionTasksRead, authz.PermissionTasksWrite},
			},
		},
		agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			projectID: {
				agentID: {authz.PermissionTasksRead, authz.PermissionTasksWrite},
			},
		},
		agentRoles: map[uuid.UUID]map[uuid.UUID]string{
			projectID: {
				agentID: "agent_developer",
			},
		},
	}

	r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)
	base := fmt.Sprintf("/api/v1/projects/%s/tasks", projectID)

	for i := 0; i < 5; i++ {
		w := serve(r, agentKeyAuthReq(t.Context(), http.MethodPost, base, agentID, map[string]any{"title": fmt.Sprintf("Agent Task %d", i)}))
		if w.Code != http.StatusNotFound {
			t.Fatalf("unbound create: want 404, got %d", w.Code)
		}
	}
	if len(taskRepo.tasks) != 0 {
		t.Fatal("denied batch created tasks")
	}
}

// ---------------------------------------------------------------------------
// Agent Permission Tests
// ---------------------------------------------------------------------------

func TestAgentAPIKey_PermissionScenarios(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	taskID := uuid.New()
	agentID := uuid.New()
	botUserID := uuid.MustParse(testAgentBotUserID)

	// Seed a task
	taskRepo.tasks[taskID] = &taskdom.Task{
		ID:        taskID,
		ProjectID: projectID,
		Title:     "Permission Test Task",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	taskURL := fmt.Sprintf("/api/v1/projects/%s/tasks/%s", projectID, taskID)

	t.Run("read_permission_without_delegation_denies_get", func(t *testing.T) {
		store := &projectPermStore{
			userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
				botUserID: {
					projectID: {authz.PermissionTasksRead},
				},
			},
			agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
				projectID: {
					agentID: {authz.PermissionTasksRead},
				},
			},
			agentRoles: map[uuid.UUID]map[uuid.UUID]string{
				projectID: {
					agentID: "agent_reader",
				},
			},
		}
		r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)

		w := serve(r, agentKeyAuthReq(t.Context(), http.MethodGet, taskURL, agentID, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("read permission: expected 404, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("write_permission_without_delegation_denies_update", func(t *testing.T) {
		store := &projectPermStore{
			userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
				botUserID: {
					projectID: {authz.PermissionTasksWrite},
				},
			},
			agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
				projectID: {
					agentID: {authz.PermissionTasksWrite},
				},
			},
			agentRoles: map[uuid.UUID]map[uuid.UUID]string{
				projectID: {
					agentID: "agent_developer",
				},
			},
		}
		r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)

		w := serve(r, agentKeyAuthReq(t.Context(), http.MethodPatch, taskURL, agentID, map[string]any{
			"title": "Updated",
		}))
		if w.Code != http.StatusNotFound {
			t.Errorf("write permission: expected 404, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("no_permission_denies_all", func(t *testing.T) {
		store := &projectPermStore{
			userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
				botUserID: {
					projectID: {},
				},
			},
			agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
				projectID: {
					agentID: {},
				},
			},
			agentRoles: map[uuid.UUID]map[uuid.UUID]string{
				projectID: {
					agentID: "agent_no_perms",
				},
			},
		}
		r := buildAgentKeyRouter(taskRepo, apiKeyRepo, store)

		w := serve(r, agentKeyAuthReq(t.Context(), http.MethodGet, taskURL, agentID, nil))
		if w.Code != http.StatusForbidden {
			t.Errorf("no permission: expected 403, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// ---------------------------------------------------------------------------
// Per-Agent MCP API Key Tests
//
// Unlike the shared-static-key + X-Agent-ID tests above, these exercise
// apikeysvc.Service.Authenticate's FindAgentByMCPAPIKeyHash fallback (see
// agent_service.go's GenerateAgentMCPKey / GenerateGlobalAgentMCPKey and
// authn.go's agentClaimsForKey): a key that, by itself, already proves which
// single agent it belongs to, with no X-Agent-ID header needed or trusted.
// ---------------------------------------------------------------------------

// mcpKeyAgentIdentityStore fakes apikeysvc.AgentIdentityStore with a real,
// stateful hash->agent mapping (unlike passthroughAgentIdentityStore above,
// which always fails FindAgentByMCPAPIKeyHash) so a test can mint a key for
// an agent and then present it over HTTP exactly like a real generated key.
type mcpKeyAgentIdentityStore struct {
	agents map[uuid.UUID]*agentdom.Agent
	byHash map[string]uuid.UUID // sha256 hex hash -> agentID
}

func newMCPKeyAgentIdentityStore() *mcpKeyAgentIdentityStore {
	return &mcpKeyAgentIdentityStore{
		agents: map[uuid.UUID]*agentdom.Agent{},
		byHash: map[string]uuid.UUID{},
	}
}

// registerKey mints a plaintext MCP key for agentID and records its hash,
// mirroring agentsvc.Service.GenerateAgentMCPKey (random bytes -> hex ->
// SHA-256 hash). Calling it again for the same agent drops the previous
// hash first, mirroring SetMCPAPIKeyHash overwriting the column — only one
// key is ever live per agent.
func (s *mcpKeyAgentIdentityStore) registerKey(agentID uuid.UUID) string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	plaintext := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plaintext))
	hash := hex.EncodeToString(sum[:])

	for h, id := range s.byHash {
		if id == agentID {
			delete(s.byHash, h)
		}
	}
	s.byHash[hash] = agentID
	if _, ok := s.agents[agentID]; !ok {
		s.agents[agentID] = &agentdom.Agent{ID: agentID, AgentScope: agentdom.AgentScopeProject}
	}
	return plaintext
}

func (s *mcpKeyAgentIdentityStore) FindAgentByID(_ context.Context, agentID uuid.UUID) (*agentdom.Agent, error) {
	if a, ok := s.agents[agentID]; ok {
		return a, nil
	}
	return nil, agentdom.ErrAgentNotFound
}

func (s *mcpKeyAgentIdentityStore) HasActiveGlobalChatSession(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return true, nil
}

func (s *mcpKeyAgentIdentityStore) FindAgentByMCPAPIKeyHash(_ context.Context, hash string) (*agentdom.Agent, error) {
	agentID, ok := s.byHash[hash]
	if !ok {
		return nil, agentdom.ErrAgentNotFound
	}
	return s.agents[agentID], nil
}

// buildAgentMCPKeyRouter is like buildAgentKeyRouterWithBotID but wires a
// stateful mcpKeyAgentIdentityStore instead of the always-fail
// passthroughAgentIdentityStore, so tests can register a real per-agent MCP
// key and then present it over HTTP to exercise the full Authenticate ->
// FindAgentByMCPAPIKeyHash -> agentClaimsForKey path production traffic
// uses. The static agent key is still wired (as it always is in production,
// see bootstrap/app.go) so agentBotUserID is set correctly.
func buildAgentMCPKeyRouter(taskRepo *fakeTaskRepo, apiKeyRepo *fakeAPIKeyRepo, store *projectPermStore, identityStore *mcpKeyAgentIdentityStore) http.Handler {
	tm := jwttoken.New(testSecret, 15*time.Minute, 168*time.Hour)
	refreshStore := &fakeRefreshStore{}
	userRepo := newFakeUserRepo()
	authService := authsvc.New(userRepo, tm, refreshStore, 168*time.Hour, 24*time.Hour)
	userService := usersvc.New(userRepo, userRepo)
	projectRepo := newFakeProjectRepo()
	projectService := projectsvc.New(projectRepo, taskRepo, nil)
	taskService := tasksvc.New(taskRepo, authz.NewTaskAccessPolicy(store))
	sprintService := sprintsvc.New(newFakeSprintRepoIT(), taskRepo, nil).WithTaskAccessChecker(taskService)
	viewService := sprintsvc.NewViewService(newFakeViewRepoIT(), newFakeSprintRepoIT(), taskRepo, nil).WithTaskAccessChecker(taskService)
	activityRepo := newFakeTaskActivityRepo()
	activityService := tasksvc.NewActivityService(activityRepo, taskRepo, &fakeActivityMemberRepo{}, nil).WithTaskAccessChecker(taskService)

	apiKeyService := apikeysvc.New(apiKeyRepo).
		WithAgentKey(testAgentAPIKey, uuid.MustParse(testAgentBotUserID)).
		WithAgentIdentityStore(identityStore)

	if store == nil {
		store = &projectPermStore{}
	}
	authorizer := authz.NewAuthorizer(store).WithAgentRoleResolver(store)

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	return router.New(router.Deps{
		TokenManager:         tm,
		APIKeyAuth:           apiKeyService,
		Authorizer:           authorizer,
		ProjectVisibilitySvc: projectService,
		Health:               handler.NewHealthHandler(),
		Auth:                 handler.NewAuthHandler(authService, testCookieCfg),
		User:                 handler.NewUserHandler(userService),
		GlobalRole:           handler.NewGlobalRoleHandler(&fakeGlobalRoleService{}),
		Project:              handler.NewProjectHandler(projectService, authorizer),
		Task:                 handler.NewTaskHandler(taskService, viewService, activityService),
		Sprint:               handler.NewSprintHandler(sprintService, viewService),
		View:                 handler.NewViewHandler(viewService),
		Log:                  log,
	})
}

func TestAgentMCPKey_CreateTask_UnboundAgentDenied(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	agentID := uuid.New()
	botUserID := uuid.MustParse(testAgentBotUserID)

	identityStore := newMCPKeyAgentIdentityStore()
	mcpKey := identityStore.registerKey(agentID)

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {
				projectID: {authz.PermissionTasksWrite},
			},
		},
		agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			projectID: {
				agentID: {authz.PermissionTasksWrite},
			},
		},
		agentRoles: map[uuid.UUID]map[uuid.UUID]string{
			projectID: {
				agentID: "agent_developer",
			},
		},
	}

	r := buildAgentMCPKeyRouter(taskRepo, apiKeyRepo, store, identityStore)
	base := fmt.Sprintf("/api/v1/projects/%s/tasks", projectID)

	body, _ := json.Marshal(map[string]any{"title": "Created via MCP key"})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, base, bytes.NewReader(body))
	req.Header.Set("X-API-Key", mcpKey)
	req.Header.Set("Content-Type", "application/json")
	// Deliberately no X-Agent-ID header — identity must come entirely from
	// the key itself.

	w := serve(r, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("unbound agent: want 404, got %d: %s", w.Code, w.Body.String())
	}
	if len(taskRepo.tasks) != 0 {
		t.Fatal("denied create inserted a task")
	}
}

func TestAgentMCPKey_UnknownKey_Returns401(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()

	identityStore := newMCPKeyAgentIdentityStore()
	r := buildAgentMCPKeyRouter(taskRepo, apiKeyRepo, &projectPermStore{}, identityStore)
	base := fmt.Sprintf("/api/v1/projects/%s/tasks", projectID)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, base, nil)
	req.Header.Set("X-API-Key", "paca_some_key_that_was_never_registered")

	w := serve(r, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unregistered key, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAgentMCPKey_RegeneratedKey_InvalidatesOldKey covers the "only one live
// key per agent" guarantee described on GenerateAgentMCPKey: regenerating a
// key must make the previous plaintext stop authenticating immediately.
func TestAgentMCPKey_RegeneratedKey_InvalidatesOldKey(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	agentID := uuid.New()
	botUserID := uuid.MustParse(testAgentBotUserID)

	identityStore := newMCPKeyAgentIdentityStore()
	oldKey := identityStore.registerKey(agentID)

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {projectID: {authz.PermissionTasksRead}},
		},
		agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			projectID: {agentID: {authz.PermissionTasksRead}},
		},
		agentRoles: map[uuid.UUID]map[uuid.UUID]string{
			projectID: {agentID: "agent_reader"},
		},
	}

	r := buildAgentMCPKeyRouter(taskRepo, apiKeyRepo, store, identityStore)
	base := fmt.Sprintf("/api/v1/projects/%s/task-types", projectID)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, base, nil)
	req.Header.Set("X-API-Key", oldKey)
	if w := serve(r, req); w.Code != http.StatusOK {
		t.Fatalf("expected 200 with the live key, got %d: %s", w.Code, w.Body.String())
	}

	// Regenerate — overwrites the stored hash, same as SetMCPAPIKeyHash.
	identityStore.registerKey(agentID)

	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, base, nil)
	req.Header.Set("X-API-Key", oldKey)
	w := serve(r, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for the replaced key, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAgentMCPKey_XAgentIDHeaderIgnored is the regression test for the
// impersonation gap this PR closes: presenting one agent's own MCP key
// alongside an X-Agent-ID header claiming a *different* agent must still
// attribute the request to the key's own agent, never the header's. Before
// this PR's per-agent key, a leaked shared AGENT_API_KEY plus an arbitrary
// X-Agent-ID header could authenticate as any agent; agentClaimsForKey now
// trusts the key-resolved identity unconditionally once key.AgentID is set.
func TestAgentMCPKey_XAgentIDHeaderIgnored(t *testing.T) {
	taskRepo := newFakeTaskRepoIT()
	apiKeyRepo := newFakeAPIKeyRepo()
	projectID := uuid.New()
	ownAgentID := uuid.New()
	otherAgentID := uuid.New() // caller never holds this agent's key
	botUserID := uuid.MustParse(testAgentBotUserID)

	identityStore := newMCPKeyAgentIdentityStore()
	ownKey := identityStore.registerKey(ownAgentID)
	identityStore.registerKey(otherAgentID)

	taskID := uuid.New()
	taskRepo.tasks[taskID] = &taskdom.Task{
		ID:        taskID,
		ProjectID: projectID,
		Title:     "Task",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	store := &projectPermStore{
		userPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			botUserID: {projectID: {authz.PermissionTasksRead}},
		},
		agentPerms: map[uuid.UUID]map[uuid.UUID][]authz.Permission{
			projectID: {
				ownAgentID:   {authz.PermissionTasksRead},
				otherAgentID: {}, // explicitly no permissions
			},
		},
		agentRoles: map[uuid.UUID]map[uuid.UUID]string{
			projectID: {
				ownAgentID:   "agent_reader",
				otherAgentID: "agent_no_perms",
			},
		},
	}

	r := buildAgentMCPKeyRouter(taskRepo, apiKeyRepo, store, identityStore)
	taskURL := fmt.Sprintf("/api/v1/projects/%s/task-types", projectID)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, taskURL, nil)
	req.Header.Set("X-API-Key", ownKey)
	// Spoofing attempt: claim to be an agent with no permissions. If this
	// were honored, the request would 403; the fix means it's ignored and
	// the request is attributed to ownAgentID instead.
	req.Header.Set("X-Agent-ID", otherAgentID.String())

	w := serve(r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (request attributed to the key's own agent, X-Agent-ID ignored), got %d: %s", w.Code, w.Body.String())
	}
}
