package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Paca-AI/paca/apps/acp-bridge/internal/acpclient"
)

const privateRecoveryError = "Private conversation could not be restored safely; please retry or contact the local bridge operator."

// PrivateHermesConfig is trusted local deployment configuration, never wire data.
// The pinned launch script owns native configuration and the OS sandbox.
type PrivateHermesConfig struct {
	Root     string
	Template string
	Launcher string
	Python   string
	Source   string
}

func (c Config) Validate() error {
	if c.PrivateHermes == nil {
		return nil
	}
	if c.AgentID == "" || c.InstanceID == "" {
		return errors.New("private Hermes requires authenticated agent and stable instance IDs")
	}
	p := c.PrivateHermes
	for _, path := range []string{p.Root, p.Template, p.Source, filepath.Dir(p.Launcher)} {
		if err := rejectSymlinkPath(path); err != nil {
			return err
		}
	}
	for _, path := range []string{p.Launcher, p.Python} {
		if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
			return errors.New("private executable paths must be absolute")
		}
		for _, part := range strings.Split(path, string(filepath.Separator)) {
			if part == ".." {
				return errors.New("private executable path contains traversal")
			}
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("private executable must be a regular file")
		}
	}
	info, err := os.Lstat(p.Launcher)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private launcher cannot be a symlink")
	}
	return nil
}

func (p PrivateHermesConfig) command(key string) ([]string, []string) {
	return []string{p.Python, "-I", p.Launcher, "--root", p.Root, "--template", p.Template, "--python", p.Python, "--source", p.Source},
		[]string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=en_US.UTF-8", "PACA_PRIVATE_KEY=" + key}
}

func (r *Runner) privateIdentityFor(msg map[string]any) (privateIdentity, bool, error) {
	id := privateIdentity{}
	for field, dest := range map[string]*string{"instance_id": &id.InstanceID, "agent_id": &id.AgentID, "project_id": &id.ProjectID, "owner_user_id": &id.OwnerUserID, "conversation_id": &id.ConversationID} {
		value, ok := msg[field].(string)
		if !ok {
			return id, false, fmt.Errorf("invalid private identity field %s", field)
		}
		*dest = value
	}
	prior, ok := msg["has_prior_turns"].(bool)
	if !ok || msg["audience"] != "owner_private" || id.AgentID != r.cfg.AgentID || id.InstanceID != r.cfg.InstanceID {
		return id, false, errors.New("private dispatch identity does not match trusted bridge")
	}
	return id, prior, id.validate()
}

func (r *Runner) privateCWD(id privateIdentity) string {
	return filepath.Join(r.cfg.PrivateHermes.Root, "runtimes", id.key(), "workspace")
}

func (r *Runner) preparePrivate(id privateIdentity, prior bool) (*sessionBinding, error) {
	if err := r.cfg.Validate(); err != nil {
		return nil, err
	}
	cwd := r.privateCWD(id)
	if err := validateBindingPaths(r.cfg.PrivateHermes.Root, id, cwd); err != nil {
		return nil, err
	}
	// Only the trusted bridge/launcher may write the parents of each workspace.
	if err := os.MkdirAll(cwd, 0700); err != nil {
		return nil, err
	}
	binding, err := loadSessionBinding(r.cfg.PrivateHermes.Root, id, cwd)
	if os.IsNotExist(err) && !prior {
		return nil, nil
	}
	return binding, err
}

func (r *Runner) setupTimeout() time.Duration {
	if r.cfg.SetupTimeout > 0 {
		return r.cfg.SetupTimeout
	}
	return 60 * time.Second
}

func (r *Runner) turnError(state *conversationState, err error) string {
	if state.private != nil {
		return privateRecoveryError
	}
	return err.Error()
}

// Admission and forwarding share updateMu, so a callback admitted for one turn
// cannot spill across its finish/reset boundary. mu is never held over ACP RPCs.
func (r *Runner) dispatchUpdate(state *conversationState, generation uint64, u acpclient.Update) {
	state.updateMu.Lock()
	defer state.updateMu.Unlock()
	state.mu.Lock()
	if generation != state.generation || !state.turnRunning || u.SessionID != state.sessionID {
		state.mu.Unlock()
		return
	}
	if state.loading {
		if (u.Kind == "user_message_chunk" || u.Kind == "agent_message_chunk") && strings.TrimSpace(extractChunkText(u.Raw)) != "" {
			state.loadedHistory = true
		}
		state.mu.Unlock()
		return
	}
	enabled := state.updatesEnabled
	cid, pid := state.currentConversationID, state.currentProjectID
	private := state.private != nil
	state.mu.Unlock()
	if !enabled {
		return
	}
	// Native Hermes sends deferred session-context usage after load without a turn
	// identity. Only Prompt's response accounting belongs to this private turn.
	if private && u.Kind == "usage_update" {
		return
	}
	r.handleUpdate(state, cid, pid, u)
}

// Close stops setup and active turns, closes every child, and ends the sweeper.
// Durable bindings and native homes deliberately survive shutdown and eviction.
func (r *Runner) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	if r.done != nil {
		close(r.done)
	}
	var clients []*acpclient.Client
	var turns []chan struct{}
	for _, state := range r.conversations {
		state.mu.Lock()
		if state.turnCancel != nil {
			state.turnCancel()
		}
		if state.turnDone != nil {
			turns = append(turns, state.turnDone)
		}
		if state.client != nil {
			clients = append(clients, state.client)
			state.client = nil
		}
		state.generation++
		state.mu.Unlock()
	}
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, client := range clients {
		wg.Add(1)
		go func(c *acpclient.Client) { defer wg.Done(); _ = c.Close() }(client)
	}
	wg.Wait()
	for _, done := range turns {
		<-done
	}
}
