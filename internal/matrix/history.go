package matrix

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

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
