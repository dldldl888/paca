package e2e_test

import (
	"context"
	"github.com/Paca-AI/api/internal/platform/authz"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	"github.com/google/uuid"
	"net/http"
	"testing"
	"time"
)

func TestAuthorizationWriterRechecksActorAfterCoordinatorWait(t *testing.T) {
	env := newE2EEnv(t)
	managerClient, managerToken, project, editorRole := seedACPUser(t, env)
	pid := uuid.MustParse(project)
	var adminRole uuid.UUID
	if err := env.db.Get(&adminRole, `SELECT id FROM project_roles WHERE project_id=$1 AND role_name='Admin'`, pid); err != nil {
		t.Fatal(err)
	}
	seedUser(t, env, "stale-writer", "password1", "Stale Writer")
	u, err := env.userRepo.FindByUsername(env.ctx, "stale-writer")
	if err != nil {
		t.Fatal(err)
	}
	member := uuid.New()
	if _, err = env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type)VALUES($1,$2,$3,$4,'human')`, member, pid, u.ID, adminRole); err != nil {
		t.Fatal(err)
	}
	attacker, token := taskMemberLogin(t, env, "stale-writer", "password1")
	d := pg.NewAuthorizationDelivery(env.db, env.redisClient, uuid.NewString())
	d.Install()
	entered, release := make(chan struct{}), make(chan struct{})
	adminDone := make(chan error, 1)
	go func() {
		adminDone <- d.Mutate(env.ctx, func(ctx context.Context) error {
			close(entered)
			<-release
			_, e := pg.DeliveryQuery(ctx, env.db).ExecContext(ctx, `UPDATE project_members SET project_role_id=$1 WHERE id=$2`, editorRole, member)
			return e
		})
	}()
	<-entered
	req := mustRequest(env.ctx, t, "PATCH", env.base+"/api/v1/projects/"+project+"/members/"+member.String(), jsonBody(t, map[string]any{"project_role_id": adminRole}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	response := make(chan *http.Response, 1)
	requestError := make(chan error, 1)
	go func() {
		resp, e := attacker.Do(req)
		if e != nil {
			requestError <- e
			return
		}
		response <- resp
	}()
	// PostgreSQL itself proves the request has passed middleware and is waiting
	// for the coordinator held by the demotion, without an application test hook.
	deadline := time.After(5 * time.Second)
	for {
		var waiting int
		if e := env.db.Get(&waiting, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory'`); e != nil {
			t.Fatal(e)
		}
		if waiting > 0 {
			break
		}
		select {
		case e := <-requestError:
			t.Fatal(e)
		case resp := <-response:
			resp.Body.Close()
			t.Fatalf("request never reached coordinator: %d", resp.StatusCode)
		case <-deadline:
			t.Fatal("request did not wait on coordinator")
		case <-time.After(time.Millisecond):
		}
	}
	close(release)
	if e := <-adminDone; e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-requestError:
		t.Fatal(e)
	case resp := <-response:
		defer resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("stale pre-lock allow changed role: HTTP %d", resp.StatusCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("denied write did not return")
	}
	var role uuid.UUID
	if err = env.db.Get(&role, `SELECT project_role_id FROM project_members WHERE id=$1`, member); err != nil || role.String() != editorRole {
		t.Fatal("stale writer restored admin", role, err)
	}
	// The still-authorized manager remains able to perform the same route action.
	req = mustRequest(env.ctx, t, "PATCH", env.base+"/api/v1/projects/"+project+"/members/"+member.String(), jsonBody(t, map[string]any{"project_role_id": adminRole}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+managerToken)
	resp := mustDo(t, managerClient, req)
	defer resp.Body.Close()
	assertStatus(t, resp, 200)
	// A stale full-row self-profile write cannot undo a subsequent global demotion.
	stale, err := env.userRepo.FindByID(env.ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherRole := uuid.New()
	if _, err = env.db.Exec(`INSERT INTO global_roles(id,name,permissions)VALUES($1,'demoted-fixture','[]')`, otherRole); err != nil {
		t.Fatal(err)
	}
	if _, err = env.db.Exec(`UPDATE users SET role_id=$1 WHERE id=$2`, otherRole, u.ID); err != nil {
		t.Fatal(err)
	}
	stale.FullName = "late profile"
	if err = env.userRepo.Update(authz.WithActorID(env.ctx, u.ID), stale); err == nil {
		t.Fatal("stale profile restored earlier global role")
	}
}
