package e2e_test

import (
	"fmt"
	automationdom "github.com/Paca-AI/api/internal/domain/automation"
	"github.com/Paca-AI/api/internal/events"
	"github.com/Paca-AI/api/internal/worker"
	"github.com/google/uuid"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestAutomationPauseStepPersistencePostgres(t *testing.T) {
	env := newE2EEnv(t)
	group := "e2e.persistence." + uuid.NewString()
	c := worker.NewAutomationConsumer(env.redisClient, env.automationRepo, env.taskRepo, env.taskSvc, env.activitySvc, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithExecutionValidator(env.automationSvc).WithAgentMessaging(env.projectRepo, env.agentSvc).WithConsumerGroup(group)
	c.Start(env.ctx)
	defer c.Stop()
	seedTaskMemberUser(t, env, "persistence-manager", "password1")
	client, token := taskMemberLogin(t, env, "persistence-manager", "password1")
	pid := createProjectForTasksViaAPI(t, env, client, token)
	role := projectRoleIDByName(t, env, client, token, pid, "Editor")
	_, created := createAgentRequest(t, env, client, token, pid, llmAgentBody(role, "persistence-agent", nil))
	member := memberIDForAgent(listProjectMembersViaAPI(t, env, client, token, pid), idOf(assertDataMap(t, created)))
	for _, kind := range []string{"wait", "trigger_ai_agent"} {
		for _, fail := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/reject=%t", kind, fail), func(t *testing.T) {
				task := createTaskViaAPI(t, env, client, token, pid, "before pause")
				aid := createAutomationViaAPI(t, env, client, token, pid, "persistence "+kind)
				tr := addAutomationNodeViaAPI(t, env, client, token, pid, aid, "trigger", "api_trigger", map[string]any{"target_task_id": task})
				cfg := map[string]any{"wait_minutes": 1}
				if kind == "trigger_ai_agent" {
					cfg = map[string]any{"member_id": member}
				}
				pause := addAutomationNodeViaAPI(t, env, client, token, pid, aid, "action", kind, cfg)
				next := addAutomationNodeViaAPI(t, env, client, token, pid, aid, "action", "update_task", map[string]any{"update": map[string]any{"title": "after pause"}})
				addAutomationEdgeViaAPI(t, env, client, token, pid, aid, idOf(tr), idOf(pause))
				addAutomationEdgeViaAPI(t, env, client, token, pid, aid, idOf(pause), idOf(next))
				activateAutomationViaAPI(t, env, client, token, pid, aid)
				if fail {
					// nextval is not rolled back when this INSERT is rejected: only the first write fails.
					_, err := env.db.Exec(fmt.Sprintf(`CREATE SEQUENCE pause_failure_once;
 CREATE FUNCTION reject_pause_once() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.node_id='%s'::uuid AND nextval('pause_failure_once')=1 THEN RAISE EXCEPTION 'one-shot pause evidence failure'; END IF;
 RETURN NEW; END $$;
 CREATE TRIGGER reject_pause_once BEFORE INSERT ON automation_run_steps FOR EACH ROW EXECUTE FUNCTION reject_pause_once()`, idOf(pause)))
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if _, err := env.db.Exec(`DROP TRIGGER reject_pause_once ON automation_run_steps; DROP FUNCTION reject_pause_once(); DROP SEQUENCE pause_failure_once`); err != nil {
							t.Error(err)
						}
					}()
				}
				secret := generateWebhookTokenViaAPI(t, env, client, token, pid, aid, idOf(tr))
				postWebhookExpect(t, env, idOf(tr), secret, 202)
				var runID uuid.UUID
				var status string
				deadline := time.Now().Add(12 * time.Second)
				for time.Now().Before(deadline) {
					err := env.db.Get(&runID, `SELECT id FROM automation_runs WHERE automation_id=$1`, aid)
					if err == nil {
						run, e := env.automationRepo.FindRunByID(env.ctx, runID)
						if e != nil {
							t.Fatal(e)
						}
						status = string(run.Status)
						var n int
						_ = env.db.Get(&n, `SELECT (SELECT count(*) FROM automation_pending_delays WHERE run_id=$1)+(SELECT count(*) FROM automation_pending_agent_waits WHERE run_id=$1)`, runID)
						if (fail && status != "running") || (!fail && n == 1) {
							break
						}
					}
					time.Sleep(20 * time.Millisecond)
				}
				if runID == uuid.Nil {
					t.Fatal("run never started")
				}
				if fail && status != "failed" {
					t.Errorf("one-shot insert error reported status %q", status)
				}
				// Wait for the actual external-trigger delivery to be acknowledged, proving it will not replay.
				acknowledged := false
				for time.Now().Before(deadline) {
					groups, err := env.redisClient.XInfoGroups(env.ctx, events.StreamAutomationExternalTriggers).Result()
					if err != nil {
						t.Fatal(err)
					}
					for _, g := range groups {
						if g.Name == group && g.Pending == 0 && g.Lag == 0 {
							acknowledged = true
						}
					}
					if acknowledged {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				if !acknowledged {
					t.Error("external trigger left pending for replay")
				}
				var pending, steps, runs int
				for query, dst := range map[string]*int{
					`SELECT (SELECT count(*) FROM automation_pending_delays WHERE run_id=$1)+(SELECT count(*) FROM automation_pending_agent_waits WHERE run_id=$1)`: &pending,
					`SELECT count(*) FROM automation_run_steps WHERE run_id=$1`:                                                                                     &steps,
				} {
					if err := env.db.Get(dst, query, runID); err != nil {
						t.Fatal(err)
					}
				}
				if err := env.db.Get(&runs, `SELECT count(*) FROM automation_runs WHERE automation_id=$1`, aid); err != nil {
					t.Fatal(err)
				}
				if runs != 1 {
					t.Fatalf("trigger replayed %d runs", runs)
				}
				if fail && (pending != 0 || steps != 0) {
					t.Errorf("unproven pending/evidence: %d/%d", pending, steps)
				}
				if !fail && (pending != 1 || steps != 1) {
					t.Errorf("control pause missing: %d/%d", pending, steps)
				}
				var convs []uuid.UUID
				if err := env.db.Select(&convs, `SELECT id FROM agent_conversations WHERE task_id=$1`, task); err != nil {
					t.Fatal(err)
				}
				want := 0
				if kind == "trigger_ai_agent" {
					want = 1
				}
				if len(convs) != want {
					t.Fatalf("already-dispatched conversations %d want %d", len(convs), want)
				}
				if fail {
					// Later completion writes must not erase the failed outcome.
					now := time.Now()
					if err := env.automationRepo.UpdateRun(env.ctx, &automationdom.Run{ID: runID, Status: automationdom.RunStatusCompleted, FinishedAt: &now}); err != nil {
						t.Fatal(err)
					}
					run, err := env.automationRepo.FindRunByID(env.ctx, runID)
					if err != nil {
						t.Fatal(err)
					}
					if run.Status != automationdom.RunStatusFailed {
						t.Error("later finalization overwrote failure")
					}
					if len(convs) > 0 {
						publishAgentConversationStatusViaRedis(t, env, convs[0].String(), "finished")
					}
				} else if kind == "wait" {
					if _, err := env.db.Exec(`UPDATE automation_pending_delays SET resume_at=NOW()-INTERVAL '1 second' WHERE run_id=$1`, runID); err != nil {
						t.Fatal(err)
					}
					scheduler := worker.NewWaitScheduler(env.redisClient, c, slog.New(slog.NewTextHandler(io.Discard, nil))).WithInterval(20 * time.Millisecond)
					scheduler.Start(env.ctx)
					defer scheduler.Stop()
				} else {
					publishAgentConversationStatusViaRedis(t, env, convs[0].String(), "finished")
				}
				if !fail {
					waitForTaskField(t, env, client, token, pid, task, 10*time.Second, func(data map[string]any) bool { return data["title"] == "after pause" })
				}
				if kind == "trigger_ai_agent" {
					// Observe the terminal-status message being consumed before asserting no continuation.
					consumed := false
					until := time.Now().Add(5 * time.Second)
					for time.Now().Before(until) {
						groups, err := env.redisClient.XInfoGroups(env.ctx, events.StreamAgentConversationStatus).Result()
						if err != nil {
							t.Fatal(err)
						}
						for _, g := range groups {
							if g.Name == group && g.Pending == 0 && g.Lag == 0 {
								consumed = true
							}
						}
						if consumed {
							break
						}
						time.Sleep(20 * time.Millisecond)
					}
					if !consumed {
						t.Fatal("terminal conversation event not consumed")
					}
					var after int
					if err := env.db.Get(&after, `SELECT count(*) FROM agent_conversations WHERE task_id=$1`, task); err != nil {
						t.Fatal(err)
					}
					if after != 1 {
						t.Fatalf("terminal event redispatched conversations: %d", after)
					}
				}
				got, err := env.taskRepo.FindTaskByID(env.ctx, uuid.MustParse(task))
				if err != nil {
					t.Fatal(err)
				}
				if fail && got.Title != "before pause" {
					t.Fatal("downstream replayed")
				}
			})
		}
	}
}
