package runner

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// privateIdentity comes from the authenticated backend's persisted conversation.
// Task IDs and configurable shared session scopes never participate in this key.
type privateIdentity struct {
	InstanceID     string `json:"instance_id"`
	AgentID        string `json:"agent_id"`
	ProjectID      string `json:"project_id"` // empty only for an explicitly global private conversation
	OwnerUserID    string `json:"owner_user_id"`
	ConversationID string `json:"conversation_id"`
}

func (id privateIdentity) key() string {
	data, _ := json.Marshal(id)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (id privateIdentity) validate() error {
	for _, field := range []string{id.InstanceID, id.AgentID, id.OwnerUserID, id.ConversationID} {
		if field == "" {
			return errors.New("missing private conversation identity")
		}
	}
	for _, field := range []string{id.InstanceID, id.AgentID, id.ProjectID, id.OwnerUserID, id.ConversationID} {
		if len(field) > 128 || strings.TrimSpace(field) != field || strings.ContainsRune(field, 0) {
			return errors.New("invalid private conversation identity")
		}
	}
	return nil
}

type sessionBinding struct {
	Version   int             `json:"version"`
	Identity  privateIdentity `json:"identity"`
	SessionID string          `json:"session_id"`
	CWD       string          `json:"cwd"`
}

// rejectSymlinkPath also checks not-yet-created paths without following existing
// symlink ancestors. The trusted parent is outside every model sandbox's writes.
func rejectSymlinkPath(path string) error {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return errors.New("private runtime path must be absolute")
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return errors.New("private runtime path contains traversal")
		}
	}
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
			return errors.New("unsafe private runtime directory")
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}

func validateBindingPaths(root string, id privateIdentity, cwd string) error {
	if err := id.validate(); err != nil {
		return err
	}
	if err := rejectSymlinkPath(root); err != nil {
		return err
	}
	if cwd != filepath.Join(root, "runtimes", id.key(), "workspace") {
		return errors.New("private session cwd does not match identity")
	}
	return rejectSymlinkPath(cwd)
}

func syncDirectory(directory *os.Root) error {
	file, err := directory.Open(".")
	if err != nil {
		return err
	}
	err = file.Sync()
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func openBindings(root string, create bool) (*os.Root, error) {
	directory, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	if create {
		if err := directory.Mkdir("bindings", 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
	}
	info, err := directory.Lstat("bindings")
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("unsafe private binding directory")
	}
	if create {
		if err := syncDirectory(directory); err != nil {
			return nil, err
		}
	}
	return directory.OpenRoot("bindings")
}

func readSessionBinding(directory *os.Root, name string) (*sessionBinding, error) {
	info, err := directory.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 65536 {
		return nil, errors.New("unsafe private session binding")
	}
	file, err := directory.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 65537))
	decoder.DisallowUnknownFields()
	var binding sessionBinding
	if err := decoder.Decode(&binding); err != nil {
		return nil, errors.New("invalid private session binding")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid trailing private binding data")
	}
	return &binding, nil
}

func loadSessionBinding(root string, id privateIdentity, cwd string) (*sessionBinding, error) {
	if err := validateBindingPaths(root, id, cwd); err != nil {
		return nil, err
	}
	directory, err := openBindings(root, false)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	binding, err := readSessionBinding(directory, id.key()+".json")
	if err != nil {
		return nil, err
	}
	if binding.Version != 1 || binding.Identity != id || binding.CWD != cwd || strings.TrimSpace(binding.SessionID) == "" || len(binding.SessionID) > 4096 || strings.ContainsRune(binding.SessionID, 0) {
		return nil, errors.New("private session binding does not match its conversation")
	}
	if err := syncDirectory(directory); err != nil {
		return nil, err
	}
	return binding, nil
}

func saveSessionBinding(root string, id privateIdentity, cwd, sessionID string) error {
	if err := validateBindingPaths(root, id, cwd); err != nil {
		return err
	}
	if strings.TrimSpace(sessionID) == "" || len(sessionID) > 4096 || strings.ContainsRune(sessionID, 0) {
		return errors.New("invalid native session identity")
	}
	directory, err := openBindings(root, true)
	if err != nil {
		return err
	}
	defer directory.Close()
	name := id.key() + ".json"
	binding := sessionBinding{Version: 1, Identity: id, SessionID: sessionID, CWD: cwd}
	if old, err := readSessionBinding(directory, name); !os.IsNotExist(err) {
		if err != nil {
			return err
		}
		if *old != binding {
			return errors.New("refusing to replace an existing private session binding")
		}
		return syncDirectory(directory)
	}
	data, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	temporary := "." + rand.Text() + ".tmp"
	file, err := directory.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer directory.Remove(temporary)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Link atomically commits a complete file without replacing a concurrently
	// committed binding. Rename would silently overwrite another session's ID.
	if err := directory.Link(temporary, name); err != nil {
		return fmt.Errorf("commit private session binding: %w", err)
	}
	if err := directory.Remove(temporary); err != nil {
		return err
	}
	return syncDirectory(directory)
}
