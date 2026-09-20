package e2e_test

import (
	"context"
	"github.com/Paca-AI/api/internal/platform/authz"
	jwttoken "github.com/Paca-AI/api/internal/platform/token"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	docsvc "github.com/Paca-AI/api/internal/service/doc"
	"github.com/Paca-AI/api/internal/transport/http/handler"
	middleware "github.com/Paca-AI/api/internal/transport/http/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type heldDocumentFlush struct {
	http.ResponseWriter
	entered, release chan struct{}
	once             *sync.Once
}

func (w heldDocumentFlush) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w heldDocumentFlush) FlushError() error {
	w.once.Do(func() { close(w.entered); <-w.release })
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func TestDocumentJSONDeliveryFlushesBeforeRevocationCommit(t *testing.T) {
	env := newE2EEnv(t)
	coordinator := startFileDelivery(t, env)
	seedTaskMemberUser(t, env, "doc-flush", "password1")
	client, token := taskMemberLogin(t, env, "doc-flush", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	user, _ := env.userRepo.FindByUsername(env.ctx, "doc-flush")
	id := uuid.New()
	if _, e := env.db.Exec(`INSERT INTO documents(id,project_id,title,content,visibility,owner_user_id)VALUES($1,$2,'flush secret','[]','private',$3)`, id, pid, user.ID); e != nil {
		t.Fatal(e)
	}
	repo := pg.NewDocumentRepository(env.db)
	doc := handler.NewDocumentHandler(docsvc.New(repo, env.projectRepo), docsvc.NewActivityService(repo, repo, env.projectRepo, nil)).WithDeliveryGate(env.fileGate)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	releaseFlush := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFlush()
	mux := chi.NewRouter()
	mux.Use(middleware.Authn(jwttoken.New(e2eJWTSecret, e2eAccessTTL, e2eRefreshTTL)))
	mux.Get("/projects/{projectId}/docs/{docId}", func(w http.ResponseWriter, r *http.Request) {
		doc.GetDocument(heldDocumentFlush{w, entered, release, &once}, r)
	})
	server := httptest.NewServer(mux)
	defer func() { releaseFlush(); server.Close() }()
	req := mustRequest(env.ctx, t, "GET", server.URL+"/projects/"+pid.String()+"/docs/"+id.String(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	delivered := make(chan error, 1)
	go func() {
		resp, e := http.DefaultClient.Do(req)
		if e == nil {
			_, e = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		delivered <- e
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("document JSON never flushed within the delivery lease")
	}
	revoked := make(chan error, 1)
	go func() { revoked <- repo.DeleteDocument(authz.WithActorID(context.Background(), user.ID), id) }()
	deadline := time.After(3 * time.Second)
	for !env.fileGate.State().Blocked {
		select {
		case <-deadline:
			t.Fatal("revocation did not enter barrier")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case e := <-revoked:
		t.Fatalf("authority committed before buffered JSON flushed: %v", e)
	default:
	}
	releaseFlush()
	if e := <-delivered; e != nil {
		t.Fatal(e)
	}
	if e := <-revoked; e != nil {
		t.Fatal(e)
	}
	waitFileGate(t, env)
	state, e := coordinator.State(env.ctx)
	if e != nil || state.Blocked {
		t.Fatalf("gate not reopened %+v %v", state, e)
	}
	req = mustRequest(env.ctx, t, "GET", env.base+"/api/v1/projects/"+pid.String()+"/docs/"+id.String(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := mustDo(t, client, req)
	resp.Body.Close()
	assertStatus(t, resp, 404)
}

func TestDocumentAccessMutationRechecksAfterCoordinatorWait(t *testing.T) {
	env := newE2EEnv(t)
	coordinator := startFileDelivery(t, env)
	seedTaskMemberUser(t, env, "doc-barrier-manager", "password1")
	mc, mt := taskMemberLogin(t, env, "doc-barrier-manager", "password1")
	pid := createProjectForTasksViaAPI(t, env, mc, mt)
	seedUser(t, env, "doc-barrier-writer", "password1", "Writer")
	writer, _ := env.userRepo.FindByUsername(env.ctx, "doc-barrier-writer")
	member := uuid.New()
	var admin, editor uuid.UUID
	if e := env.db.Get(&admin, `SELECT id FROM project_roles WHERE project_id=$1 AND role_name='Admin'`, pid); e != nil {
		t.Fatal(e)
	}
	if e := env.db.Get(&editor, `SELECT id FROM project_roles WHERE project_id=$1 AND role_name='Editor'`, pid); e != nil {
		t.Fatal(e)
	}
	if _, e := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type)VALUES($1,$2,$3,$4,'human')`, member, pid, writer.ID, admin); e != nil {
		t.Fatal(e)
	}
	wc, wt := taskMemberLogin(t, env, "doc-barrier-writer", "password1")
	id := uuid.New()
	if _, e := env.db.Exec(`INSERT INTO documents(id,project_id,title,visibility,owner_user_id)VALUES($1,$2,'barrier doc','private',$3)`, id, pid, writer.ID); e != nil {
		t.Fatal(e)
	}
	conn, e := env.db.Conn(env.ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	if _, e = conn.ExecContext(env.ctx, `SELECT pg_advisory_lock(734228905116)`); e != nil {
		t.Fatal(e)
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(734228905116)`)
	request := func(c *http.Client, token, path string, body any) <-chan int {
		r := mustRequest(env.ctx, t, "PATCH", env.base+path, jsonBody(t, body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		result := make(chan int, 1)
		go func() {
			resp, e := c.Do(r)
			if e != nil {
				result <- 0
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			result <- resp.StatusCode
		}()
		return result
	}
	waitQueued := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			if e := env.db.Get(&n, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory'`); e != nil {
				t.Fatal(e)
			}
			if n >= want {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("coordinator queue not reached")
	}
	demotion := request(mc, mt, "/api/v1/projects/"+pid+"/members/"+member.String(), map[string]any{"project_role_id": editor})
	waitQueued(1)
	scope := request(wc, wt, "/api/v1/projects/"+pid+"/docs/"+id.String()+"/access", map[string]any{"visibility": "project", "expected_access_version": 1})
	waitQueued(2)
	if _, e = conn.ExecContext(env.ctx, `SELECT pg_advisory_unlock(734228905116)`); e != nil {
		t.Fatal(e)
	}
	if status := <-demotion; status != 200 {
		t.Fatalf("demotion status=%d", status)
	}
	if status := <-scope; status != 403 {
		t.Fatalf("stale writer expanded scope status=%d", status)
	}
	var visibility string
	var version int64
	if e = env.db.QueryRow(`SELECT visibility,access_version FROM documents WHERE id=$1`, id).Scan(&visibility, &version); e != nil {
		t.Fatal(e)
	}
	if visibility != "private" || version != 1 {
		t.Fatal("stale writer changed private audience")
	}
	state, e := coordinator.State(env.ctx)
	if e != nil || !state.Blocked {
		t.Fatalf("outer permission recheck must preserve existing fail-closed behavior %+v %v", state, e)
	}
}
