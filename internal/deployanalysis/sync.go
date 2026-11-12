package deployanalysis

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/abdorrahmani/phelix/internal/deploy"
)

// Offline synchronization queue. A report is queued when it is stored locally
// and removed only after the backend accepted it, so a deployment performed
// offline is synchronized on a later command without duplicates. Delivery is
// idempotent on analysis_id at the backend, which makes a retried ack harmless.

const syncFileName = "deployment_sync.jsonl"

func syncPath(appName string) (string, error) {
	dir, err := deploy.AppDataDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, syncFileName), nil
}

func enqueueSyncLocked(appName string, rep *Report) error {
	path, err := syncPath(appName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// SyncBatch returns unsent reports, newest identity wins. Bad lines are
// skipped: a crash while appending must not block synchronization forever.
func SyncBatch(appName string, limit int) ([]Report, error) {
	var out []Report
	err := withStoreLock(appName, syncFileName, func() error {
		list, err := loadSyncLocked(appName)
		if err != nil {
			return err
		}
		seen := make(map[string]bool)
		for i := len(list) - 1; i >= 0; i-- {
			if seen[list[i].AnalysisID] {
				continue
			}
			seen[list[i].AnalysisID] = true
			out = append(out, list[i])
			if limit > 0 && len(out) >= limit {
				break
			}
		}
		return nil
	})
	return out, err
}

// AckSynced removes delivered reports from the queue.
func AckSynced(appName string, analysisIDs []string) error {
	if len(analysisIDs) == 0 {
		return nil
	}
	return withStoreLock(appName, syncFileName, func() error {
		list, err := loadSyncLocked(appName)
		if err != nil {
			return err
		}
		ack := make(map[string]bool, len(analysisIDs))
		for _, id := range analysisIDs {
			ack[id] = true
		}
		remaining := list[:0]
		for _, rep := range list {
			if !ack[rep.AnalysisID] {
				remaining = append(remaining, rep)
			}
		}
		path, err := syncPath(appName)
		if err != nil {
			return err
		}
		if len(remaining) == 0 {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), ".phelix-sync-*.tmp")
		if err != nil {
			return err
		}
		name := tmp.Name()
		defer func() { _ = os.Remove(name) }()
		enc := json.NewEncoder(tmp)
		for i := range remaining {
			if err := enc.Encode(&remaining[i]); err != nil {
				tmp.Close()
				return err
			}
		}
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		return os.Rename(name, path)
	})
}

func loadSyncLocked(appName string) ([]Report, error) {
	path, err := syncPath(appName)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Report
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var rep Report
		if json.Unmarshal(scanner.Bytes(), &rep) != nil || rep.AnalysisID == "" {
			continue
		}
		out = append(out, rep)
	}
	return out, scanner.Err()
}

// HasPendingSync reports whether appName has reports awaiting delivery. It is
// a single stat, safe to call on every monitor tick.
func HasPendingSync(appName string) bool {
	path, err := syncPath(appName)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}
