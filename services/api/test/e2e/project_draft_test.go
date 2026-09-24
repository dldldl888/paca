package e2e_test

import (
	"net/http"
	"testing"

	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/google/uuid"
)

func TestPrivateProjectDraftStaysOutsideProjectsAndUsesVersionCAS(t *testing.T) {
	env := newE2EEnv(t)
	seedProjectAdminUser(t, env, "project-draft-owner", "password1")
	seedProjectAdminUser(t, env, "project-draft-other", "password1")
	client, token := projectAdminLogin(t, env, "project-draft-owner", "password1")
	otherClient, otherToken := projectAdminLogin(t, env, "project-draft-other", "password1")
	owner, err := env.userRepo.FindByUsername(env.ctx, "project-draft-owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := env.userRepo.FindByUsername(env.ctx, "project-draft-other")
	if err != nil {
		t.Fatal(err)
	}

	// A global private chat session is the only durable anchor accepted by the
	// service. The draft itself must remain detached from formal projects.
	agentID := uuid.New()
	if _, err := env.db.Exec(`INSERT INTO agents(id,name,handle,agent_type,agent_scope,acp_provider,llm_provider,llm_model,llm_api_key_secret) VALUES($1,'Project draft anchor','project-draft-anchor','acp','global','claude-code','','','')`, agentID); err != nil {
		t.Fatal(err)
	}
	session, _, err := env.agentSvc.StartGlobalChatSession(authz.WithActorID(env.ctx, owner.ID), agentID, owner.ID, "anchor", nil, "")
	if err != nil {
		t.Fatal(err)
	}

	var before int
	if err := env.db.Get(&before, "SELECT count(*) FROM projects"); err != nil {
		t.Fatal(err)
	}
	body := jsonBody(t, map[string]any{
		"name":            "私有项目草稿",
		"goal":            "在正式创建项目之前完成范围确认",
		"manager_user_id": owner.ID.String(),
		"participants": []any{map[string]any{
			"user_id": owner.ID.String(),
			"role":    "负责人",
		}},
		"materials":       []any{},
		"chat_session_id": session.ID.String(),
	})
	req := mustRequest(env.ctx, t, http.MethodPost, env.base+"/api/v1/project-drafts", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp := mustDo(t, client, req)
	defer resp.Body.Close()
	assertStatus(t, resp, http.StatusCreated)
	var created envelope
	decodeJSON(t, resp, &created)
	draft := assertDataMap(t, created)
	if draft["version"] != float64(1) {
		t.Fatalf("initial draft version: %v", draft["version"])
	}
	if _, err := uuid.Parse(draft["id"].(string)); err != nil {
		t.Fatalf("draft id: %v", err)
	}

	var after int
	if err := env.db.Get(&after, "SELECT count(*) FROM projects"); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("private draft created formal project: before=%d after=%d", before, after)
	}

	path := env.base + "/api/v1/project-drafts/" + draft["id"].(string)
	update := map[string]any{
		"name":            "私有项目草稿（已编辑）",
		"goal":            "在正式创建项目之前完成范围确认",
		"manager_user_id": owner.ID.String(),
		"participants": []any{
			map[string]any{"user_id": owner.ID.String(), "role": "负责人"},
			map[string]any{"user_id": other.ID.String(), "role": "协作人"},
		},
		"materials": []any{map[string]any{
			"id":       uuid.NewString(),
			"kind":     "document",
			"purpose":  "范围背景",
			"task_ids": []any{},
			"required": true,
		}},
		"expected_version": 1,
	}
	req = mustRequest(env.ctx, t, http.MethodPatch, path, jsonBody(t, update))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp = mustDo(t, client, req)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	req = mustRequest(env.ctx, t, http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp = mustDo(t, client, req)
	assertStatus(t, resp, http.StatusOK)
	var refreshed envelope
	decodeJSON(t, resp, &refreshed)
	refreshedDraft := assertDataMap(t, refreshed)
	if refreshedDraft["version"] != float64(2) || refreshedDraft["name"] != "私有项目草稿（已编辑）" {
		t.Fatalf("refresh did not restore latest draft: %v", refreshedDraft)
	}
	resp.Body.Close()

	req = mustRequest(env.ctx, t, http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+otherToken)
	resp = mustDo(t, otherClient, req)
	assertStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()

	req = mustRequest(env.ctx, t, http.MethodPatch, path, jsonBody(t, update))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp = mustDo(t, client, req)
	assertStatus(t, resp, http.StatusConflict)
	resp.Body.Close()
}
