package runauth

import (
	"context"
	"encoding/json"
	"github.com/Paca-AI/agent-runner/internal/agent"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthorizeStrictResponseAndTrustedIdentity(t *testing.T) {
	cid, aid, instance, owner, pid := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	valid := map[string]any{"conversation_id": cid, "agent_id": aid, "instance_id": instance, "owner_user_id": owner, "project_id": pid, "audience": "owner_private", "has_prior_turns": false, "status": "running", "task_id": "", "comment_id": "", "chat_session_id": ""}
	cases := []struct {
		name   string
		mutate func(map[string]any)
		status int
		pass   bool
	}{{"valid", func(map[string]any) {}, 200, true}, {"global_private", func(m map[string]any) { m["project_id"] = "" }, 200, true}}
	for key := range valid {
		key := key
		cases = append(cases, struct {
			name   string
			mutate func(map[string]any)
			status int
			pass   bool
		}{"missing_" + key, func(m map[string]any) { delete(m, key) }, 200, false}, struct {
			name   string
			mutate func(map[string]any)
			status int
			pass   bool
		}{"null_" + key, func(m map[string]any) { m[key] = nil }, 200, false})
	}
	for _, value := range []string{"wrong", uuid.Nil.String()} {
		value := value
		cases = append(cases, struct {
			name   string
			mutate func(map[string]any)
			status int
			pass   bool
		}{"bad_owner_" + value, func(m map[string]any) { m["owner_user_id"] = value }, 200, false})
	}
	for _, code := range []int{401, 404, 500} {
		cases = append(cases, struct {
			name   string
			mutate func(map[string]any)
			status int
			pass   bool
		}{http.StatusText(code), func(map[string]any) {}, code, false})
	}
	cases = append(cases, struct {
		name   string
		mutate func(map[string]any)
		status int
		pass   bool
	}{"extra_field", func(m map[string]any) { m["role"] = "admin" }, 200, false}, struct {
		name   string
		mutate func(map[string]any)
		status int
		pass   bool
	}{"wrong_boolean", func(m map[string]any) { m["has_prior_turns"] = "false" }, 200, false}, struct {
		name   string
		mutate func(map[string]any)
		status int
		pass   bool
	}{"wrong_agent", func(m map[string]any) { m["agent_id"] = uuid.NewString() }, 200, false})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string]any{}
			for k, v := range valid {
				data[k] = v
			}
			tc.mutate(data)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/internal/agent-runs/authorize" || r.Method != "POST" || r.Header.Get("X-Internal-Token") != "synthetic" {
					t.Error("wrong authenticated endpoint")
				}
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if len(body) != 3 || body["conversation_id"] != cid || body["agent_id"] != aid || body["begin_turn"] != true {
					t.Error("unexpected request fields")
				}
				w.WriteHeader(tc.status)
				json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
			}))
			defer srv.Close()
			a, e := New(srv.URL, "synthetic", instance).Authorize(context.Background(), uuid.MustParse(cid), uuid.MustParse(aid), true)
			if (e == nil) != tc.pass {
				t.Fatalf("pass=%v authorization=%+v err=%v", tc.pass, a, e)
			}
			if tc.pass {
				spoof := uuid.New()
				trigger := agent.Trigger{AgentID: spoof, ProjectID: spoof, ActorUserID: &spoof, ActorMemberID: &spoof, TaskID: &spoof, CommentID: &spoof, ChatSessionID: &spoof}
				a.Apply(&trigger)
				if trigger.AgentID.String() != aid || trigger.ActorUserID.String() != owner || trigger.ActorMemberID != nil || trigger.TaskID != nil || trigger.CommentID != nil || trigger.ChatSessionID != nil {
					t.Fatal("queue identity survived trusted replacement")
				}
			}
		})
	}
}
