package matrix

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Local Matrix Run history: one JSON file per run under
// <phelix data dir>/matrix/runs/. This is the smallest mechanism that makes
// `phelix matrix list` / `phelix matrix show` reliable — deliberately not a
// database, and deliberately local-only. The future Remote Matrix phase will
// persist the same Run entities backend-side; nothing here is remote-aware.

// RunsDir returns the directory holding matrix run records. It follows the
// same data-dir resolution as the rest of Phelix: PHELIX_DATA_DIR, then the
// Docker volume, then ~/.phelix.
func RunsDir() (string, error) {
	base := os.Getenv("PHELIX_DATA_DIR")
	if base == "" {
		if _, err := os.Stat("/.dockerenv"); err == nil {
			base = "/var/lib/phelix"
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot locate the phelix data directory", err)
			}
			base = filepath.Join(home, ".phelix")
		}
	}
	return filepath.Join(base, "matrix", "runs"), nil
}

// SaveRun persists one finished run as <id>.json. It fails when a run with
// the same ID already exists so a minted ID is never silently reused.
func SaveRun(run *Run) error {
	if run == nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "matrix: cannot save a nil run")
	}
	if _, err := ParseRunID(string(run.ID)); err != nil {
		return err
	}
	dir, err := RunsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "create matrix runs directory")
	}

	path := filepath.Join(dir, string(run.ID)+".json")
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "encode matrix run", err)
	}
	const flags = os.O_CREATE | os.O_EXCL | os.O_WRONLY
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return phelixerr.Newf(phelixerr.CodeAlreadyExists, "matrix run %s already exists", run.ID)
		}
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "write matrix run %s", run.ID)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "write matrix run %s", run.ID)
	}
	return nil
}

// LoadRun reads one run by ID. A missing or malformed record is a clean
// CodeNotFound — `phelix matrix show` turns it into an actionable error
// instead of a stack trace.
func LoadRun(id RunID) (*Run, error) {
	if _, err := ParseRunID(string(id)); err != nil {
		return nil, err
	}
	dir, err := RunsDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, string(id)+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, phelixerr.Newf(phelixerr.CodeNotFound,
				"matrix run %s not found — run 'phelix matrix list' to see available runs", id)
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "read matrix run %s", id)
	}
	var run Run
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "matrix run %s is malformed", id)
	}
	if run.ID != id {
		return nil, phelixerr.Newf(phelixerr.CodeConfiguration,
			"matrix run %s is malformed: record claims ID %s", id, run.ID)
	}
	return &run, nil
}

// ListRuns returns all persisted runs, newest first (ties broken by ID so the
// order is deterministic). Malformed files are skipped, not fatal — one bad
// record must never hide the rest of the history.
func ListRuns() (runs []*Run, skipped int, err error) {
	dir, err := RunsDir()
	if err != nil {
		return nil, 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "read matrix runs directory")
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		// Per-run release manifests live in the same directory
		// (<id>.manifest.json) but are not run records.
		if strings.HasSuffix(entry.Name(), ".manifest.json") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if rerr != nil {
			skipped++
			continue
		}
		var run Run
		if jerr := json.Unmarshal(data, &run); jerr != nil || run.ID == "" {
			skipped++
			continue
		}
		runs = append(runs, &run)
	}

	sort.SliceStable(runs, func(i, j int) bool {
		if !runs[i].StartedAt.Equal(runs[j].StartedAt) {
			return runs[i].StartedAt.After(runs[j].StartedAt)
		}
		return runs[i].ID > runs[j].ID
	})
	return runs, skipped, nil
}

// historyMu serializes history writes within this process: incremental run
// updates (one per completed combination) and finalizations must never
// interleave mid-file.
var historyMu sync.Mutex

// UpdateRun rewrites an existing run record atomically (temp file + rename),
// preserving the create-only guarantee of SaveRun for new runs. It refuses to
// rewrite a run whose persisted status is terminal: finished runs are
// immutable history — a manual retry must never modify its source run.
func UpdateRun(run *Run) error {
	if run == nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "matrix: cannot update a nil run")
	}
	if _, err := ParseRunID(string(run.ID)); err != nil {
		return err
	}
	dir, err := RunsDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, string(run.ID)+".json")

	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "encode matrix run", err)
	}

	historyMu.Lock()
	defer historyMu.Unlock()

	existing, err := LoadRun(run.ID)
	if err != nil {
		return err
	}
	if IsTerminalRunStatus(existing.Status) {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix run %s is already %s and cannot be modified — retry it with 'phelix matrix retry %s --failed' instead",
			run.ID, existing.Status, run.ID)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "write matrix run %s", run.ID)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "replace matrix run %s", run.ID)
	}
	return nil
}

// AcquireRunLock takes the execution lock for a run: <runsdir>/<id>.lock
// containing the owning PID. It fails when the lock is held by a live
// process, which is what prevents two concurrent executions (two resumes, or
// a resume next to a still-running build) from executing the same
// combinations. A lock left behind by a dead process (crash, SIGKILL, machine
// restart) is stale and gets reclaimed — that is what makes resume safe after
// a restart. The returned release function removes the lock.
func AcquireRunLock(id RunID) (release func(), err error) {
	if _, err := ParseRunID(string(id)); err != nil {
		return nil, err
	}
	dir, err := RunsDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "create matrix runs directory")
	}
	path := filepath.Join(dir, string(id)+".lock")

	for i := 0; i < 3; i++ {
		f, oerr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if oerr == nil {
			_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
			_ = f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !os.IsExist(oerr) {
			return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, oerr, "lock matrix run %s", id)
		}

		// Locked already: only a dead owner's lock may be reclaimed.
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, rerr, "read lock for matrix run %s", id)
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
		if perr != nil || !processAlive(pid) {
			// Stale lock (unreadable or dead owner). Best-effort removal,
			// then retry the acquisition.
			os.Remove(path)
			continue
		}
		return nil, phelixerr.Newf(phelixerr.CodeDeployLocked,
			"matrix run %s is being executed by process %d — wait for it to finish or stop that process before resuming", id, pid)
	}
	return nil, phelixerr.Newf(phelixerr.CodeDeployLocked, "matrix run %s is locked by another execution", id)
}

// processAlive reports whether a PID belongs to a live process. Signal 0
// probes without delivering anything; EPERM means alive but owned by another
// user.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// LatestResumableRun returns the newest run that has incomplete combinations
// (status running — orphaned — or interrupted). When appName is non-empty,
// only runs of that application are considered.
func LatestResumableRun(appName string) (*Run, error) {
	runs, _, err := ListRuns()
	if err != nil {
		return nil, err
	}
	for _, r := range runs { // newest first
		if appName != "" && r.AppName != appName {
			continue
		}
		if r.Resumable() {
			return r, nil
		}
	}
	return nil, phelixerr.Newf(phelixerr.CodeNotFound,
		"no resumable matrix run found%s — see 'phelix matrix list'", resumableSuffix(appName))
}

func resumableSuffix(appName string) string {
	if appName == "" {
		return ""
	}
	return " for application " + appName
}

// RunLockOwner reports whether a live process currently holds the run's
// execution lock, and which PID owns it. A missing lock — or a stale one left
// by a dead process — means the run is not being executed right now, even if
// its persisted status still says "running" (an orphaned run: resumable, but
// not active).
func RunLockOwner(id RunID) (executing bool, pid int) {
	if _, err := ParseRunID(string(id)); err != nil {
		return false, 0
	}
	dir, err := RunsDir()
	if err != nil {
		return false, 0
	}
	raw, rerr := os.ReadFile(filepath.Join(dir, string(id)+".lock"))
	if rerr != nil {
		return false, 0
	}
	owner, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if perr != nil || !processAlive(owner) {
		return false, 0
	}
	return true, owner
}

// ActiveRuns returns the Matrix Runs currently being executed by a live
// process: persisted status "running" and an execution lock held by a live
// PID. Newest first (ListRuns order), so the deterministic "current" run of
// several concurrent executions is the most recently started one.
func ActiveRuns() ([]*Run, error) {
	runs, _, err := ListRuns()
	if err != nil {
		return nil, err
	}
	active := make([]*Run, 0)
	for _, r := range runs {
		if r.Status != RunStatusRunning {
			continue
		}
		if executing, _ := RunLockOwner(r.ID); executing {
			active = append(active, r)
		}
	}
	return active, nil
}
