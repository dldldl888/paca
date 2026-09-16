package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// This explicit native probe never asks a model to generate. It exercises the
// real Go spawn/store/load path with real empty, persisted and deleted rows.
func TestPrivateNativeRestoration(t *testing.T) {
	if os.Getenv("PACA_NATIVE_RECOVERY_TEST") != "1" {
		t.Skip("set PACA_NATIVE_RECOVERY_TEST=1 for installed native Hermes")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := "/Users/xxp/.hermes/hermes-agent"
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(cwd, "scripts", "launch_hermes.py")
	template := os.Getenv("PACA_NATIVE_HERMES_TEMPLATE")
	if template == "" {
		t.Fatal("native test requires the trusted PACA_NATIVE_HERMES_TEMPLATE directory")
	}

	cfg := Config{AgentID: "native-agent", InstanceID: "native-instance", SetupTimeout: 30 * time.Second, PrivateHermes: &PrivateHermesConfig{Root: root, Template: template, Launcher: launcher, Python: filepath.Join(source, "venv/bin/python"), Source: source}}
	id := privateIdentity{InstanceID: cfg.InstanceID, AgentID: cfg.AgentID, OwnerUserID: "native-owner", ProjectID: "native-project", ConversationID: "native-conversation"}
	newState := func() *conversationState {
		return &conversationState{private: &id, chunks: newChunkBuffer(), turnRunning: true, currentConversationID: id.ConversationID, currentProjectID: id.ProjectID}
	}
	r, sent := privateRunner(t, cfg)
	state := newState()
	client, sid, err := r.ensureSession(context.Background(), state, id.ConversationID, id.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if sid == "" {
		t.Fatal("native new returned no session")
	}
	if _, err := loadSessionBinding(root, id, r.privateCWD(id)); err != nil {
		t.Fatal("native new did not commit binding:", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	// Actual empty persisted row returns matching provenance but no replay.
	for _, stage := range []string{"empty", "history", "deleted"} {
		t.Run(stage, func(t *testing.T) {
			{
				script := `import sys; from hermes_state import SessionDB; db=SessionDB(); db.append_message(sys.argv[1], 'user', 'native historical user'); db.append_message(sys.argv[1], 'assistant', 'native historical assistant')`
				if stage == "empty" {
					script = `import os,sys; from hermes_state import SessionDB; db=SessionDB(); db.create_session(sys.argv[1], 'acp', model_config={'cwd':os.getcwd()}); assert db.get_messages_as_conversation(sys.argv[1]) == []`
				}
				if stage == "deleted" {
					script = `import os,sys,sqlite3; db=sqlite3.connect(os.path.join(os.environ['HERMES_HOME'],'state.db')); db.execute('DELETE FROM sessions WHERE id=?',(sys.argv[1],)); db.commit()`
				}
				cmd := exec.Command("/usr/bin/sandbox-exec", "-f", filepath.Join(root, "profiles", id.key()+".sb"), cfg.PrivateHermes.Python, "-c", script, sid)
				cmd.Dir = r.privateCWD(id)
				cmd.Env = []string{"PATH=/usr/bin:/bin", "PYTHONPATH=" + source, "HERMES_HOME=" + filepath.Join(root, "runtimes", id.key(), "home"), "PYTHONPYCACHEPREFIX=" + filepath.Join(root, "runtimes", id.key(), "pycache")}
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("native seed %s failed: %v: %s", stage, err, output)
				}
			}
			rebuilt, events := privateRunner(t, cfg)
			s := newState()
			restored, loaded, err := rebuilt.ensureSession(context.Background(), s, id.ConversationID, id.ProjectID)
			if stage == "history" {
				if err != nil || loaded != sid {
					t.Fatalf("native load: %s %v", loaded, err)
				}
				if err := restored.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil || restored != nil {
					t.Fatal("empty/deleted native recovery accepted")
				}
				t.Logf("%s native recovery refused: %v", stage, err)
			}
			if len(events.all()) != 0 {
				t.Fatal("native replay forwarded")
			}
		})
	}
	if len(sent.all()) != 0 {
		t.Fatal("native setup emitted events")
	}
}
