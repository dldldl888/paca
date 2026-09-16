package e2e_test

import (
	"context"
	"encoding/json"
	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/delivery"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestAuthorizationDeliveryCommitFailsRevokedRunsAndRecoversQueue(t *testing.T) {
	env := newE2EEnv(t)
	client, token, project, role := seedACPUser(t, env)
	pid := uuid.MustParse(project)
	_, result := createAgentRequest(t, env, client, token, project, acpAgentBody(role, "delivery-runs", map[string]any{"parallelism_limit": 1}))
	aid := uuid.MustParse(assertDataMap(t, result)["id"].(string))
	var manager uuid.UUID
	if err := env.db.Get(&manager, `SELECT user_id FROM project_members WHERE project_id=$1 AND member_type='human'`, pid); err != nil {
		t.Fatal(err)
	}
	managerCtx := authz.WithActorID(env.ctx, manager)
	var users, members [2]uuid.UUID
	for i, name := range []string{"run-delivery-a", "run-delivery-b"} {
		seedUser(t, env, name, "password1", name)
		u, e := env.userRepo.FindByUsername(env.ctx, name)
		if e != nil {
			t.Fatal(e)
		}
		users[i] = u.ID
		members[i] = uuid.New()
		if _, e = env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) VALUES($1,$2,$3,$4,'human')`, members[i], pid, u.ID, role); e != nil {
			t.Fatal(e)
		}
	}
	task, err := env.taskSvc.CreateTask(managerCtx, taskdom.CreateTaskInput{ProjectID: pid, Title: "A private context", AssigneeIDs: []uuid.UUID{members[0]}})
	if err != nil {
		t.Fatal(err)
	}
	btask, err := env.taskSvc.CreateTask(managerCtx, taskdom.CreateTaskInput{ProjectID: pid, Title: "B lawful context", AssigneeIDs: []uuid.UUID{members[1]}})
	if err != nil {
		t.Fatal(err)
	}
	instance := uuid.New()
	gate := delivery.NewGate()
	d := pg.NewAuthorizationDelivery(env.db, env.redisClient, instance.String())
	env.agentSvc.WithAuthorizer(authz.NewAuthorizer(pg.NewAuthzPermissionStore(env.db))).WithPrivateRuns(instance, env.agentRepo, nil, nil, env.automationSvc).WithDeliveryGate(gate)
	d.Reconcile = env.agentSvc.ReconcilePrivateRuns
	stop, err := d.StartParticipant(env.ctx, gate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stop(ctx)
	}()
	if err = d.Mutate(env.ctx, nil); err != nil {
		t.Fatal(err)
	}
	waitOpen := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(env.ctx, 5*time.Second)
		defer cancel()
		for gate.State().Blocked {
			select {
			case <-ctx.Done():
				t.Fatal("API gate did not open")
			case <-time.After(time.Millisecond):
			}
		}
	}
	waitOpen()
	d.Install()
	start := func(i int, tid uuid.UUID) *agentdom.AgentConversation {
		t.Helper()
		_, c, e := env.agentSvc.StartChatSession(authz.WithActorID(env.ctx, users[i]), pid, aid, members[i], "hello", nil, nil, []agentdom.ContextItemRef{{Type: agentdom.ContextItemTask, ID: tid.String()}}, "queue")
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	a, queued, b := start(0, task.ID), start(0, task.ID), start(1, btask.ID)
	assertState := func(id uuid.UUID, want string) {
		t.Helper()
		c, e := env.agentRepo.FindConversationByID(env.ctx, id)
		if e != nil || c.Status != want {
			t.Fatalf("conversation %s: %+v %v; want %s", id, c, e, want)
		}
	}
	assertState(a.ID, "running")
	assertState(queued.ID, "queued")
	assertState(b.ID, "queued")
	boot := uuid.NewString()
	if _, err = d.Enroll(env.ctx, boot, "runner"); err != nil {
		t.Fatal(err)
	}
	sub := env.redisClient.Subscribe(env.ctx, d.ControlChannel())
	defer sub.Close()
	if _, err = sub.Receive(env.ctx); err != nil {
		t.Fatal(err)
	}
	ack := func(kind, epoch string) {
		t.Helper()
		data, _ := json.Marshal(map[string]string{"type": kind, "epoch": epoch, "boot_id": boot})
		if e := env.redisClient.Publish(env.ctx, d.AckChannel(), data).Err(); e != nil {
			t.Fatal(e)
		}
	}
	next := func() struct{ Type, Epoch string } {
		t.Helper()
		ctx, cancel := context.WithTimeout(env.ctx, 5*time.Second)
		defer cancel()
		msg, e := sub.ReceiveMessage(ctx)
		if e != nil {
			t.Fatal(e)
		}
		var control struct{ Type, Epoch string }
		if e = json.Unmarshal([]byte(msg.Payload), &control); e != nil {
			t.Fatal(e)
		}
		return control
	}
	mutationCtx, cancelMutation := context.WithCancel(managerCtx)
	done := make(chan error, 1)
	assignees := []uuid.UUID{members[1]}
	go func() {
		_, e := env.taskSvc.UpdateTask(mutationCtx, pid, task.ID, taskdom.UpdateTaskInput{AssigneeIDs: &assignees})
		done <- e
	}()
	close := next()
	if close.Type != "close" {
		t.Fatal(close)
	}
	assertState(a.ID, "running")
	assertState(queued.ID, "queued")
	var oldAssignee uuid.UUID
	if err = env.db.Get(&oldAssignee, `SELECT member_id FROM task_assignees WHERE task_id=$1`, task.ID); err != nil || oldAssignee != members[0] {
		t.Fatal("authority changed before close ACK", err)
	}
	ack("closed", close.Epoch)
	reconcile := next()
	if reconcile.Type != "reconcile" {
		t.Fatal(reconcile)
	}
	assertState(a.ID, "failed")
	assertState(queued.ID, "failed")
	assertState(b.ID, "queued")
	var pending int
	if err = env.db.Get(&pending, `SELECT count(*) FROM agent_pending_triggers WHERE conversation_id IN($1,$2)`, a.ID, queued.ID); err != nil || pending != 0 {
		t.Fatal("revoked queue survived commit", pending, err)
	}
	// Simulate coordinator loss after business COMMIT, before native reconciliation ACK.
	cancelMutation()
	if e := <-done; e == nil {
		t.Fatal("missing native ACK accepted")
	}
	state, e := d.State(env.ctx)
	if e != nil || !state.Blocked {
		t.Fatal("unacknowledged native stop opened", state, e)
	}
	recovered := pg.NewAuthorizationDelivery(env.db, env.redisClient, instance.String())
	recovered.Reconcile = env.agentSvc.ReconcilePrivateRuns
	go func() { done <- recovered.Mutate(env.ctx, nil) }()
	close = next()
	if close.Type != "close" {
		t.Fatal(close)
	}
	ack("closed", close.Epoch)
	reconcile = next()
	if reconcile.Type != "reconcile" {
		t.Fatal(reconcile)
	}
	assertState(a.ID, "failed")
	assertState(b.ID, "queued")
	ack("reconciled", reconcile.Epoch)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	waitOpen()
	if e = d.Unenroll(env.ctx, boot); e != nil {
		t.Fatal(e)
	}
	count, e := env.agentSvc.AdvanceQueue(env.ctx, aid, 1)
	if e != nil || count != 1 {
		t.Fatal("lawful B did not advance", count, e)
	}
	assertState(b.ID, "running")
	if _, e = env.agentSvc.GetConversation(authz.WithActorID(env.ctx, users[0]), pid, a.ID, members[0]); e != nil {
		t.Fatal("lawful owner history lost", e)
	}
}
