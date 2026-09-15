package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func bindingFixture(t *testing.T) (string, privateIdentity, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := privateIdentity{InstanceID: "instance", AgentID: "agent", ProjectID: "project", OwnerUserID: "owner", ConversationID: "conversation"}
	return root, id, filepath.Join(root, "runtimes", id.key(), "workspace")
}

func TestUS012AtomicBinding(t *testing.T) {
	root, id, cwd := bindingFixture(t)
	if err := saveSessionBinding(root, id, cwd, "native-session"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "bindings", id.key()+".json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("binding must be 0600: %v %v", info, err)
	}
	loaded, err := loadSessionBinding(root, id, cwd)
	if err != nil || loaded.SessionID != "native-session" || loaded.Identity != id {
		t.Fatalf("binding roundtrip: %#v %v", loaded, err)
	}
	if err := saveSessionBinding(root, id, cwd, "native-session"); err != nil {
		t.Fatalf("retrying the identical binding: %v", err)
	}
	// A bad identity/cwd cannot replace a previously committed binding.
	if err := saveSessionBinding(root, id, filepath.Join(root, "other"), "other-session"); err == nil {
		t.Fatal("accepted a noncanonical runtime cwd")
	}
	if err := saveSessionBinding(root, id, cwd, ""); err == nil {
		t.Fatal("accepted empty native session")
	}
	if err := saveSessionBinding(root, id, cwd, "replacement"); err == nil {
		t.Fatal("replaced a previously committed native session")
	}
	after, _ := os.ReadFile(path)
	if string(original) != string(after) {
		t.Fatal("invalid save changed existing metadata")
	}
	// An unsafe destination is refused without damaging its contents.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(path, "retained")
	if err := os.WriteFile(sentinel, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := saveSessionBinding(root, id, cwd, "second-session"); err == nil {
		t.Fatal("expected filesystem commit failure")
	}
	after, _ = os.ReadFile(sentinel)
	if string(original) != string(after) {
		t.Fatal("commit failure damaged prior destination")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("partial temporary binding left behind: %v %v", entries, err)
	}
	// Competing native creations must never overwrite the one committed ID.
	concurrentRoot, concurrentID, concurrentCWD := bindingFixture(t)
	var writers sync.WaitGroup
	committed := make(chan string, 8)
	for i := range 8 {
		writers.Go(func() {
			session := fmt.Sprintf("native-%d", i)
			if err := saveSessionBinding(concurrentRoot, concurrentID, concurrentCWD, session); err == nil {
				committed <- session
			}
		})
	}
	writers.Wait()
	close(committed)
	if len(committed) != 1 {
		t.Fatalf("expected one committed session, got %d", len(committed))
	}
	winner := <-committed
	loaded, err = loadSessionBinding(concurrentRoot, concurrentID, concurrentCWD)
	if err != nil || loaded.SessionID != winner {
		t.Fatalf("lost the committed winner: %#v %v", loaded, err)
	}
	entries, err = os.ReadDir(filepath.Join(concurrentRoot, "bindings"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("concurrent commit left partial files: %v %v", entries, err)
	}
}

func TestUS012BindingSyncFailure(t *testing.T) {
	if operation := os.Getenv("PACA_TEST_BINDING_SYNC_FAILURE"); operation != "" {
		// Keep injected syscall ordinals on one thread. No production I/O is mocked.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		root, id, cwd := bindingFixture(t)
		if operation != "new" {
			// Emulate the complete map visible after Link, before directory Sync.
			if err := os.Mkdir(filepath.Join(root, "bindings"), 0700); err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(sessionBinding{1, id, "native-session", cwd})
			if err := os.WriteFile(filepath.Join(root, "bindings", id.key()+".json"), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		var err error
		if operation == "load" {
			var binding *sessionBinding
			binding, err = loadSessionBinding(root, id, cwd)
			if binding != nil {
				t.Fatal("load exposed a binding before successful directory sync")
			}
		} else {
			err = saveSessionBinding(root, id, cwd, "native-session")
		}
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("%s must preserve the directory sync failure, got %v", operation, err)
		}
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("fsync fault injection uses Linux strace; native filesystem success is tested separately")
	}
	strace, err := exec.LookPath("strace")
	if err != nil {
		t.Skip("strace is required for actual fsync error injection")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for operation, ordinal := range map[string]int{"new": 3, "retry": 2, "load": 1} {
		t.Run(operation, func(t *testing.T) {
			command := exec.Command(strace, "-f", "-qq", "-yy", "-e", "trace=fsync", "-e", fmt.Sprintf("inject=fsync:error=EIO:when=%d", ordinal), executable, "-test.run=^TestUS012BindingSyncFailure$", "-test.v")
			command.Env = append(os.Environ(), "PACA_TEST_BINDING_SYNC_FAILURE="+operation)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("directory sync failure probe: %v\n%s", err, output)
			}
			t.Logf("%s", output)
		})
	}
}

func TestUS012BindingOwnership(t *testing.T) {
	root, id, cwd := bindingFixture(t)
	if _, err := loadSessionBinding(root, id, cwd); !os.IsNotExist(err) {
		t.Fatalf("missing mapping: %v", err)
	}
	if err := saveSessionBinding(root, id, cwd, "native-session"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "bindings", id.key()+".json")
	original, _ := os.ReadFile(path)
	var good sessionBinding
	if err := json.Unmarshal(original, &good); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"InstanceID", "AgentID", "ProjectID", "OwnerUserID", "ConversationID"} {
		t.Run(field, func(t *testing.T) {
			bad := good
			reflect.ValueOf(&bad.Identity).Elem().FieldByName(field).SetString("other")
			data, _ := json.Marshal(bad)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadSessionBinding(root, id, cwd); err == nil {
				t.Fatal("accepted foreign identity")
			}
		})
	}
	for _, mutate := range []func(*sessionBinding){
		func(b *sessionBinding) { b.Version++ },
		func(b *sessionBinding) { b.CWD = filepath.Join(root, "other") },
		func(b *sessionBinding) { b.SessionID = "" },
	} {
		bad := good
		mutate(&bad)
		data, _ := json.Marshal(bad)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSessionBinding(root, id, cwd); err == nil {
			t.Fatal("accepted invalid metadata")
		}
	}
	for _, data := range []string{"{", string(original) + "{}", strings.Repeat(" ", 65537)} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSessionBinding(root, id, cwd); err == nil {
			t.Fatal("accepted incomplete/extra/oversized metadata")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.json")
	if err := os.WriteFile(outside, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSessionBinding(root, id, cwd); err == nil {
		t.Fatal("accepted symlink mapping")
	}
	if err := saveSessionBinding(root, id, cwd, "replacement"); err == nil {
		t.Fatal("replaced symlink mapping")
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSessionBinding(alias, id, cwd); err == nil {
		t.Fatal("accepted symlink root")
	}
	if _, err := loadSessionBinding(root+"/../"+filepath.Base(root), id, cwd); err == nil {
		t.Fatal("accepted traversal root")
	}
	if _, err := loadSessionBinding(root, privateIdentity{}, cwd); err == nil {
		t.Fatal("accepted missing identity")
	}
}
