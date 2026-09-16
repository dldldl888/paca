package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Paca-AI/paca/apps/acp-bridge/internal/acpclient"
)

func privateFixture(t *testing.T) (Config, map[string]any) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(root, "fake-launcher.py")
	if err := os.WriteFile(launcher, nil, 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Scope: ScopeAgent, AgentID: "agent", InstanceID: "instance", PrivateHermes: &PrivateHermesConfig{
		Root: root, Template: root, Launcher: launcher, Python: filepath.Clean(exe), Source: root,
	}, SetupTimeout: 2 * time.Second}
	msg := map[string]any{"conversation_id": "conversation", "agent_id": "agent", "instance_id": "instance", "project_id": "project", "owner_user_id": "owner", "audience": "owner_private", "has_prior_turns": false, "message": "current", "task_id": "same-task", "acp_command": []any{"/untrusted/executable"}, "work_dir": "/untrusted/cwd"}
	return cfg, msg
}

func privateRunner(t *testing.T, cfg Config) (*Runner, *capturedSends) {
	t.Helper()
	sent := newCapturedSends()
	r := New("/untrusted/workspace", cfg, nil)(sent.send).(*Runner)
	t.Cleanup(r.Close)
	return r, sent
}
func privateID(msg map[string]any) privateIdentity {
	return privateIdentity{msg["instance_id"].(string), msg["agent_id"].(string), msg["project_id"].(string), msg["owner_user_id"].(string), msg["conversation_id"].(string)}
}
func privateRPCs(t *testing.T, root string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "rpc.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}
func waitPrivateIdle(t *testing.T, r *Runner, msg map[string]any) *conversationState {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		s := r.conversations[r.sessionKeyFor(msg)]
		r.mu.Unlock()
		if s != nil {
			s.mu.Lock()
			running := s.turnRunning
			s.mu.Unlock()
			if !running {
				return s
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("private turn did not finish")
	return nil
}

func TestPrivateRecoveryAcrossProcessLoss(t *testing.T) {
	for _, loss := range []string{"rebuilt", "dead", "idle"} {
		t.Run(loss, func(t *testing.T) {
			cfg, msg := privateFixture(t)
			t.Setenv("PACA_ACP_TOKEN", "must-not-inherit")
			t.Setenv("PYTHONPATH", "must-not-inherit")
			r, sent := privateRunner(t, cfg)
			r.StartTurn(context.Background(), msg)
			waitForStatus(t, sent, "finished")
			s := waitPrivateIdle(t, r, msg)
			switch loss {
			case "rebuilt":
				r.Close()
				r, sent = privateRunner(t, cfg)
			case "dead":
				s.mu.Lock()
				c := s.client
				s.mu.Unlock()
				_ = c.Close()
			case "idle":
				r.idleTimeout = time.Second
				r.evictIdle(time.Now().Add(time.Hour))
			}
			// A stale false latch must still restore an existing map.
			r.StartTurn(context.Background(), msg)
			waitForStatus(t, sent, "finished")
			waitPrivateIdle(t, r, msg)
			want := []string{"initialize", "session/new", "session/prompt", "initialize", "session/load", "session/prompt"}
			if got := privateRPCs(t, cfg.PrivateHermes.Root); !reflect.DeepEqual(got, want) {
				t.Fatalf("RPC order %v", got)
			}
			for _, m := range sent.all() {
				if m["type"] == "event" {
					if m["event_type"] == "turn_usage" || strings.Contains(string(m["payload"].(json.RawMessage)), "historical") {
						t.Fatalf("replay event: %v", m)
					}
				}
			}
		})
	}
}

func TestPrivateIdentityRejectedBeforeSpawn(t *testing.T) {
	for _, field := range []string{"audience", "instance_id", "agent_id", "project_id", "owner_user_id", "has_prior_turns"} {
		t.Run(field, func(t *testing.T) {
			cfg, msg := privateFixture(t)
			delete(msg, field)
			r, sent := privateRunner(t, cfg)
			r.StartTurn(context.Background(), msg)
			waitForStatus(t, sent, "failed")
			if len(r.conversations) != 0 {
				t.Fatal("created state for invalid identity")
			}
			if _, err := os.Stat(filepath.Join(cfg.PrivateHermes.Root, "rpc.log")); !os.IsNotExist(err) {
				t.Fatal("spawned for invalid identity")
			}
		})
	}
	for _, field := range []string{"agent_id", "instance_id", "audience", "has_prior_turns", "project_id"} {
		t.Run("mismatch_"+field, func(t *testing.T) {
			cfg, msg := privateFixture(t)
			msg[field] = 123
			r, sent := privateRunner(t, cfg)
			r.StartTurn(context.Background(), msg)
			waitForStatus(t, sent, "failed")
			if len(r.conversations) != 0 {
				t.Fatal("invalid identity created state")
			}
		})
	}
}

func TestPrivateKeysIgnoreSharedScope(t *testing.T) {
	cfg, msg := privateFixture(t)
	for _, scope := range []string{ScopeTask, ScopeAgent, ScopeConversation} {
		cfg.Scope = scope
		r, _ := privateRunner(t, cfg)
		first := r.sessionKeyFor(msg)
		for _, field := range []string{"owner_user_id", "conversation_id", "project_id"} {
			old := msg[field]
			msg[field] = "other"
			if r.sessionKeyFor(msg) == first {
				t.Fatalf("%s merges %s", scope, field)
			}
			msg[field] = old
		}
	}
	cfg, msg = privateFixture(t)
	msg["project_id"] = ""
	r, sent := privateRunner(t, cfg)
	r.StartTurn(context.Background(), msg)
	waitForStatus(t, sent, "finished")
}

func TestPrivateRecoveryFaultsNeverPrompt(t *testing.T) {
	for _, fault := range []string{"missing_map", "corrupt_map", "empty_result", "empty_history", "foreign_history", "foreign_provenance", "no_capability", "stall_load", "stall_init", "save_failure"} {
		t.Run(fault, func(t *testing.T) {
			cfg, msg := privateFixture(t)
			root := cfg.PrivateHermes.Root
			id := privateID(msg)
			cwd := filepath.Join(root, "runtimes", id.key(), "workspace")
			if fault != "missing_map" && fault != "save_failure" {
				if err := saveSessionBinding(root, id, cwd, "saved-session"); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "missing_map" {
				msg["has_prior_turns"] = true
			}
			if fault == "corrupt_map" {
				if err := os.WriteFile(filepath.Join(root, "bindings", id.key()+".json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "behavior"), []byte(fault), 0600); err != nil {
				t.Fatal(err)
			}
			r, sent := privateRunner(t, cfg)
			r.StartTurn(context.Background(), msg)
			waitForStatus(t, sent, "failed")
			s := waitPrivateIdle(t, r, msg)
			s.mu.Lock()
			client := s.client
			s.mu.Unlock()
			if client != nil {
				t.Fatal("failed setup kept child")
			}
			for _, m := range sent.all() {
				if m["type"] == "event" {
					t.Fatalf("failed recovery emitted event %v", m)
				}
			}
			if b, _ := os.ReadFile(filepath.Join(root, "rpc.log")); strings.Contains(string(b), "session/prompt") {
				t.Fatal("prompt after recovery failure")
			}
		})
	}
}

// Reuses the runner's re-executed ACP helper; the trusted launcher arguments
// select this branch without needing inherited test-only environment variables.
func runPrivateFakeAgent() {
	root := ""
	for i, a := range os.Args {
		if a == "--root" && i+1 < len(os.Args) {
			root = os.Args[i+1]
		}
	}
	if root == "" || os.Getenv("PACA_ACP_TOKEN") != "" || os.Getenv("PYTHONPATH") != "" {
		os.Exit(91)
	}
	behaviorBytes, _ := os.ReadFile(filepath.Join(root, "behavior"))
	behavior := string(behaviorBytes)
	key := os.Getenv("PACA_PRIVATE_KEY")
	write := func(v any) { b, _ := json.Marshal(v); fmt.Println(string(b)) }
	update := func(sid, kind, text string) {
		write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sid, "update": map[string]any{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": text}, "cost": map[string]any{"amount": 9876}}}})
	}
	scanner := bufio.NewScanner(os.Stdin)
	sid := "saved-session"
	for scanner.Scan() {
		var m struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				SessionID string `json:"sessionId"`
				CWD       string `json:"cwd"`
			} `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &m) != nil {
			os.Exit(92)
		}
		f, err := os.OpenFile(filepath.Join(root, "rpc.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(93)
		}
		fmt.Fprintln(f, m.Method)
		f.Close()
		result := map[string]any{}
		switch m.Method {
		case "initialize":
			if behavior == "stall_init" {
				continue
			}
			result["agentCapabilities"] = map[string]any{"loadSession": behavior != "no_capability"}
		case "session/new":
			result["sessionId"] = sid
			if behavior == "save_failure" {
				os.WriteFile(filepath.Join(root, "bindings"), nil, 0600)
			}
		case "session/load":
			if m.Params.SessionID != sid || m.Params.CWD != filepath.Join(root, "runtimes", key, "workspace") {
				os.Exit(94)
			}
			if behavior == "stall_load" {
				continue
			}
			if behavior == "empty_result" {
				break
			}
			proof := sid
			if behavior == "foreign_provenance" {
				proof = "foreign"
			}
			result["_meta"] = map[string]any{"hermes": map[string]any{"sessionProvenance": map[string]any{"acpSessionId": proof}}}
			replayID := sid
			if behavior == "foreign_history" {
				replayID = "foreign"
			}
			if behavior != "empty_history" {
				update(replayID, "user_message_chunk", "historical user")
				update(replayID, "agent_message_chunk", "historical assistant")
			}
			for _, kind := range []string{"agent_thought_chunk", "tool_call", "tool_call_update", "session_info_update", "usage_update"} {
				update(sid, kind, "historical")
			}
		case "session/prompt":
			b, err := os.ReadFile(filepath.Join(root, "bindings", key+".json"))
			if err != nil || !strings.Contains(string(b), sid) {
				os.Exit(95)
			}
			update("foreign", "tool_call", "historical wrong session")
			update(sid, "agent_message_chunk", "current response")
			result["stopReason"] = "end_turn"
		}
		write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		if m.Method == "session/load" {
			update(sid, "usage_update", "historical deferred usage")
		}
	}
}

func TestPrivateUpdateGuardsAllKinds(t *testing.T) {
	cfg, msg := privateFixture(t)
	r, sent := privateRunner(t, cfg)
	state := &conversationState{chunks: newChunkBuffer(), turnRunning: true, sessionID: "saved", generation: 2, updatesEnabled: true, private: &privateIdentity{}}
	for _, kind := range []string{"agent_message_chunk", "agent_thought_chunk", "tool_call", "tool_call_update", "session_info_update", "usage_update"} {
		u := acpclient.Update{SessionID: "saved", Kind: kind, Raw: json.RawMessage(`{"content":{"type":"text","text":"historical"},"cost":{"amount":5}}`)}
		r.dispatchUpdate(state, 1, u)
		state.loading = true
		r.dispatchUpdate(state, 2, u)
		state.loading = false
		u.SessionID = "foreign"
		r.dispatchUpdate(state, 2, u)
		u.SessionID = "saved"
		state.turnRunning = false
		r.dispatchUpdate(state, 2, u)
		state.turnRunning = true
	}
	r.flushChunks(state, msg["conversation_id"].(string), "project")
	if len(sent.all()) != 0 || state.turnCostUSD != nil {
		t.Fatal("stale update escaped")
	}
	state.currentConversationID = msg["conversation_id"].(string)
	state.currentProjectID = "project"
	r.dispatchUpdate(state, 2, acpclient.Update{SessionID: "saved", Kind: "agent_message_chunk", Raw: json.RawMessage(`{"content":{"type":"text","text":"current response"}}`)})
	r.flushChunks(state, state.currentConversationID, state.currentProjectID)
	r.flushChunks(state, state.currentConversationID, state.currentProjectID)
	// The status is a FIFO drain marker for the asynchronous outbound queue.
	r.reportStatus(state, state.currentConversationID, state.currentProjectID, "finished", "")
	waitForStatus(t, sent, "finished")
	events := sent.all()
	if len(events) != 2 || events[0]["event_type"] != "agent_message_chunk" || extractChunkText(events[0]["payload"].(json.RawMessage)) != "current response" {
		t.Fatalf("want exactly one current assistant event before status, got %v", events)
	}
}

func TestPrivateRejectsUnsafeConfigurationAndLegacyDowngrade(t *testing.T) {
	cfg, msg := privateFixture(t)
	cfg.PrivateHermes = nil
	r, sent := privateRunner(t, cfg)
	r.StartTurn(context.Background(), msg)
	waitForStatus(t, sent, "failed")
	for _, kind := range []string{"symlink_root", "traversal_root", "missing_instance", "symlink_launcher"} {
		t.Run(kind, func(t *testing.T) {
			cfg, msg := privateFixture(t)
			switch kind {
			case "symlink_root":
				link := filepath.Join(cfg.PrivateHermes.Root, "alias")
				if err := os.Symlink(cfg.PrivateHermes.Root, link); err != nil {
					t.Fatal(err)
				}
				cfg.PrivateHermes.Root = link
			case "traversal_root":
				cfg.PrivateHermes.Root += "/../traversal"
			case "missing_instance":
				cfg.InstanceID = ""
			case "symlink_launcher":
				link := filepath.Join(cfg.PrivateHermes.Root, "launch-link")
				if err := os.Symlink(cfg.PrivateHermes.Launcher, link); err != nil {
					t.Fatal(err)
				}
				cfg.PrivateHermes.Launcher = link
			}
			if err := cfg.Validate(); err == nil {
				t.Fatal("unsafe private config accepted")
			}
			r, sent := privateRunner(t, cfg)
			r.StartTurn(context.Background(), msg)
			waitForStatus(t, sent, "failed")
			for _, m := range sent.all() {
				if m["type"] == "event" {
					t.Fatal("unsafe config emitted event")
				}
			}
		})
	}
}

func TestPrivateInterruptAndCloseDuringSetup(t *testing.T) {
	for _, action := range []string{"interrupt", "close"} {
		t.Run(action, func(t *testing.T) {
			cfg, msg := privateFixture(t)
			cfg.SetupTimeout = 30 * time.Second
			if err := os.WriteFile(filepath.Join(cfg.PrivateHermes.Root, "behavior"), []byte("stall_init"), 0600); err != nil {
				t.Fatal(err)
			}
			r, sent := privateRunner(t, cfg)
			r.StartTurn(context.Background(), msg)
			deadline := time.Now().Add(testTimeout)
			for {
				b, _ := os.ReadFile(filepath.Join(cfg.PrivateHermes.Root, "rpc.log"))
				if strings.Contains(string(b), "initialize") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no initialize")
				}
				time.Sleep(time.Millisecond)
			}
			if action == "interrupt" {
				r.Interrupt(msg["conversation_id"].(string))
			} else {
				r.Close()
			}
			s := waitPrivateIdle(t, r, msg)
			s.mu.Lock()
			client := s.client
			s.mu.Unlock()
			if client != nil {
				t.Fatal("setup cancellation retained client")
			}
			if len(sent.all()) != 0 {
				t.Fatalf("cancelled setup emitted %v", sent.all())
			}
		})
	}
}

func TestPrivateMappingTamperClosesLiveClient(t *testing.T) {
	cfg, msg := privateFixture(t)
	r, sent := privateRunner(t, cfg)
	r.StartTurn(context.Background(), msg)
	waitForStatus(t, sent, "finished")
	s := waitPrivateIdle(t, r, msg)
	if err := os.Remove(filepath.Join(cfg.PrivateHermes.Root, "bindings", privateID(msg).key()+".json")); err != nil {
		t.Fatal(err)
	}
	r.StartTurn(context.Background(), msg)
	waitForStatus(t, sent, "failed")
	waitPrivateIdle(t, r, msg)
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client != nil {
		t.Fatal("kept live client after mapping failure")
	}
	if got := privateRPCs(t, cfg.PrivateHermes.Root); !reflect.DeepEqual(got, []string{"initialize", "session/new", "session/prompt"}) {
		t.Fatalf("unexpected RPCs %v", got)
	}
}

func TestPrivateSaveBarrierPrecedesUserEvent(t *testing.T) {
	cfg, msg := privateFixture(t)
	sent := newCapturedSends()
	checked := make(chan error, 1)
	id := privateID(msg)
	cwd := filepath.Join(cfg.PrivateHermes.Root, "runtimes", id.key(), "workspace")
	r := New("/untrusted", cfg, nil)(func(ctx context.Context, event map[string]any) error {
		if event["event_type"] == "user_message" {
			_, err := loadSessionBinding(cfg.PrivateHermes.Root, id, cwd)
			checked <- err
		}
		return sent.send(ctx, event)
	}).(*Runner)
	t.Cleanup(r.Close)
	r.StartTurn(context.Background(), msg)
	waitForStatus(t, sent, "finished")
	select {
	case err := <-checked:
		if err != nil {
			t.Fatalf("user event before durable binding: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("no user event checked")
	}
}

func TestPrivateAuthorizationBarrierRejectsDelayedStart(t *testing.T) {
	cfg, msg := privateFixture(t)
	cfg.RequireAuthorization = true
	r, sent := privateRunner(t, cfg)
	msg["authorization_epoch"] = "1"
	r.StartTurn(context.Background(), msg)
	r.mu.Lock()
	n := len(r.conversations)
	r.mu.Unlock()
	if n != 0 {
		t.Fatal("unenrolled daemon started native work")
	}
	if err := r.AuthorizationBarrier(context.Background(), "open", "1", nil); err != nil {
		t.Fatal(err)
	}
	r.StartTurn(context.Background(), msg)
	waitForStatus(t, sent, "finished")
	waitPrivateIdle(t, r, msg)
	before := privateRPCs(t, cfg.PrivateHermes.Root)
	if err := r.AuthorizationBarrier(context.Background(), "close", "2", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.AuthorizationBarrier(context.Background(), "reconcile", "2", []string{"conversation"}); err != nil {
		t.Fatal(err)
	}
	if err := r.AuthorizationBarrier(context.Background(), "open", "2", nil); err != nil {
		t.Fatal(err)
	}
	r.StartTurn(context.Background(), msg)
	if got := privateRPCs(t, cfg.PrivateHermes.Root); !reflect.DeepEqual(got, before) {
		t.Fatalf("stale dispatch started RPC: %v", got)
	}
	msg["authorization_epoch"] = "2"
	r.StartTurn(context.Background(), msg)
	waitPrivateIdle(t, r, msg)
	if len(privateRPCs(t, cfg.PrivateHermes.Root)) != len(before)+1 {
		t.Fatal("lawful new epoch failed to prompt")
	}
}

func TestPrivateAuthorizationReconcileJoinsDelayedLoad(t *testing.T) {
	cfg, msg := privateFixture(t)
	first, sent := privateRunner(t, cfg)
	first.StartTurn(context.Background(), msg)
	waitForStatus(t, sent, "finished")
	waitPrivateIdle(t, first, msg)
	first.Close()
	cfg.RequireAuthorization = true
	cfg.SetupTimeout = 30 * time.Second
	if err := os.WriteFile(filepath.Join(cfg.PrivateHermes.Root, "behavior"), []byte("stall_load"), 0600); err != nil {
		t.Fatal(err)
	}
	r, output := privateRunner(t, cfg)
	msg["has_prior_turns"] = true
	msg["authorization_epoch"] = "1"
	if err := r.AuthorizationBarrier(context.Background(), "open", "1", nil); err != nil {
		t.Fatal(err)
	}
	r.StartTurn(context.Background(), msg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		rpc := privateRPCs(t, cfg.PrivateHermes.Root)
		if rpc[len(rpc)-1] == "session/load" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("load never started")
		case <-time.After(time.Millisecond):
		}
	}
	before := privateRPCs(t, cfg.PrivateHermes.Root)
	if err := r.AuthorizationBarrier(ctx, "close", "2", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.AuthorizationBarrier(ctx, "reconcile", "2", nil); err != nil {
		t.Fatal(err)
	}
	state := waitPrivateIdle(t, r, msg)
	state.mu.Lock()
	client := state.client
	state.mu.Unlock()
	if client != nil {
		t.Fatal("reconcile ACK retained native child")
	}
	if err := r.AuthorizationBarrier(ctx, "open", "2", nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, privateRPCs(t, cfg.PrivateHermes.Root)) {
		t.Fatal("late native prompt after revoked load")
	}
	if len(output.all()) != 0 {
		t.Fatal("cancelled recovery emitted content")
	}
}
