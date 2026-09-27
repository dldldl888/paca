package e2e_test

import (
	"context"
	"net/http"
	"testing"

	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/google/uuid"
)

// TestOffboardingTaskReassignment keeps the supported task reassignment path
// honest: deactivation revokes the old session, while a project administrator
// can still hand the open task to an active replacement.
func TestOffboardingTaskReassignment(t *testing.T) {
	t.Parallel()
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "handover-manager", "password1")
	managerClient, managerToken := taskMemberLogin(t, env, "handover-manager", "password1")
	projectID := uuid.MustParse(createProjectForTasksViaAPI(t, env, managerClient, managerToken))

	seedUser(t, env, "handover-leaver", "password1", "Handover Leaver")
	seedUser(t, env, "handover-replacement", "password1", "Handover Replacement")
	leaver, err := env.userRepo.FindByUsername(env.ctx, "handover-leaver")
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := env.userRepo.FindByUsername(env.ctx, "handover-replacement")
	if err != nil {
		t.Fatal(err)
	}
	leaverMember, replacementMember := uuid.New(), uuid.New()
	for memberID, userID := range map[uuid.UUID]uuid.UUID{
		leaverMember:      leaver.ID,
		replacementMember: replacement.ID,
	} {
		if _, err := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, memberID, projectID, userID); err != nil {
			t.Fatal(err)
		}
	}

	manager, err := env.userRepo.FindByUsername(env.ctx, "handover-manager")
	if err != nil {
		t.Fatal(err)
	}
	task, err := env.taskSvc.CreateTask(authz.WithActorID(env.ctx, manager.ID), taskdom.CreateTaskInput{
		ProjectID:   projectID,
		Title:       "待交接任务",
		AssigneeIDs: []uuid.UUID{leaverMember},
	})
	if err != nil {
		t.Fatal(err)
	}
	leaverClient, leaverToken := taskMemberLogin(t, env, "handover-leaver", "password1")
	replacementClient, replacementToken := taskMemberLogin(t, env, "handover-replacement", "password1")

	read := func(client *http.Client, token string, want int) {
		req := mustRequest(env.ctx, t, http.MethodGet, env.base+"/api/v1/projects/"+projectID.String()+"/tasks/"+task.ID.String(), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp := mustDo(t, client, req)
		defer resp.Body.Close()
		assertStatus(t, resp, want)
	}
	read(leaverClient, leaverToken, http.StatusOK)

	if _, err := env.db.Exec(`UPDATE users SET deleted_at=NOW() WHERE id=$1`, leaver.ID); err != nil {
		t.Fatal(err)
	}
	read(leaverClient, leaverToken, http.StatusUnauthorized)

	patch := jsonBody(t, map[string]any{"assignee_ids": []string{replacementMember.String()}})
	req := mustRequest(context.Background(), t, http.MethodPatch, env.base+"/api/v1/projects/"+projectID.String()+"/tasks/"+task.ID.String(), patch)
	req.Header.Set("Authorization", "Bearer "+managerToken)
	req.Header.Set("Content-Type", "application/json")
	resp := mustDo(t, managerClient, req)
	defer resp.Body.Close()
	assertStatus(t, resp, http.StatusOK)

	read(replacementClient, replacementToken, http.StatusOK)
}
