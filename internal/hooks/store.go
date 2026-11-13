package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

const DefaultRetention = 200

type Store struct {
	AppName   string
	Retention int
	mu        sync.Mutex
}

// NewStore returns a store for an application.
func NewStore(appName string) *Store {
	retention := DefaultRetention
	if raw := strings.TrimSpace(os.Getenv("PHELIX_HOOKS_RETENTION")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			retention = n
		}
	}
	return &Store{AppName: appName, Retention: retention}
}

// Dir returns the hook data directory for the app.
func (s *Store) Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "hooks: resolve home directory", err)
	}
	if s.AppName == "" {
		return "", phelixerr.New(phelixerr.CodeInvalidArgument, "hooks: store requires an app name")
	}
	return filepath.Join(home, ".phelix", "apps", s.AppName, "hooks"), nil
}

func (s *Store) executionsDir() (string, error) {
	dir, err := s.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "executions"), nil
}

func executionFile(id string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", "..", "_")
	return replacer.Replace(id) + ".json"
}

// Save writes one execution atomically and prunes old records.
func (s *Store) Save(exec *Execution) error {
	if exec == nil || exec.ID == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "hooks: execution record has no id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir, err := s.executionsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "hooks: create execution dir")
	}
	data, err := json.MarshalIndent(exec, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "hooks: encode execution", err)
	}
	tmp, err := os.CreateTemp(dir, ".hook-*")
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "hooks: create execution file")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "hooks: write execution")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "hooks: sync execution")
	}
	if err := tmp.Close(); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "hooks: close execution")
	}
	target := filepath.Join(dir, executionFile(exec.ID))
	if err := os.Rename(tmpName, target); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "hooks: persist execution")
	}
	s.prune(dir)
	return nil
}

// Load reads one execution by id.
func (s *Store) Load(id string) (*Execution, error) {
	dir, err := s.executionsDir()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, executionFile(id)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, phelixerr.Newf(phelixerr.CodeNotFound, "hook execution %q not found", id)
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "hooks: read execution")
	}
	var exec Execution
	if err := json.Unmarshal(raw, &exec); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "hooks: decode execution %s", id)
	}
	return &exec, nil
}

// List returns executions newest first. A missing directory means no history.
func (s *Store) List() ([]*Execution, error) {
	dir, err := s.executionsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "hooks: list executions")
	}
	out := make([]*Execution, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var exec Execution
		if err := json.Unmarshal(raw, &exec); err != nil {
			continue
		}
		out = append(out, &exec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out, nil
}

// Find returns terminal records for one deployment and hook, newest first.
func (s *Store) Find(deploymentID, hook string) ([]*Execution, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []*Execution
	for _, exec := range all {
		if exec.DeploymentID == deploymentID && (hook == "" || exec.Hook == hook) {
			out = append(out, exec)
		}
	}
	return out, nil
}

// Recover marks every execution of deploymentID that was still pending or
// running as uncertain. The process that owned them is gone, so their outcome
// cannot be established from local evidence. They are never retried
// automatically; an operator resolves them with `phelix hooks history`.
func (s *Store) Recover(deploymentID string) ([]*Execution, error) {
	if deploymentID == "" {
		return nil, nil
	}
	execs, err := s.Find(deploymentID, "")
	if err != nil {
		return nil, err
	}
	var recovered []*Execution
	for _, exec := range execs {
		if exec.Status != StatusPending && exec.Status != StatusRunning {
			continue
		}
		exec.Status = StatusUncertain
		exec.FailureReason = "execution was interrupted and its outcome cannot be established; it was not retried automatically"
		exec.FinishedAt = time.Now()
		if exec.StartedAt.IsZero() {
			exec.StartedAt = exec.FinishedAt
		}
		exec.DurationMS = exec.FinishedAt.Sub(exec.StartedAt).Milliseconds()
		if err := s.Save(exec); err != nil {
			return recovered, err
		}
		recovered = append(recovered, exec)
	}
	return recovered, nil
}

func (s *Store) prune(dir string) {
	retention := s.Retention
	if retention <= 0 {
		retention = DefaultRetention
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type fileInfo struct {
		path string
		mod  time.Time
	}
	files := make([]fileInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{path: filepath.Join(dir, entry.Name()), mod: info.ModTime()})
	}
	if len(files) <= retention {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	for _, f := range files[retention:] {
		_ = os.Remove(f.path)
	}
}
