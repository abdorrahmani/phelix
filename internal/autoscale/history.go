package autoscale

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// History kinds. A decision record is produced when the engine emits a
// non-None verdict; executed/failed record the dispatch outcome; suppressed
// records a decision the execution layer refused to attempt (a deployment
// holding the app lock, a rollout in flight, a stale decision).
const (
	HistoryDecision   = "decision"
	HistoryExecuted   = "executed"
	HistoryFailed     = "failed"
	HistorySuppressed = "suppressed"
)

// HistoryRecord is one line of an app's autoscale_history.jsonl, mirroring
// the rollback history discipline: append-only, redacted by the caller,
// best-effort (a history failure never breaks a scaling operation).
type HistoryRecord struct {
	Time      string `json:"time"`
	App       string `json:"app"`
	Kind      string `json:"kind"`
	Direction string `json:"direction,omitempty"`
	From      int    `json:"from,omitempty"`
	To        int    `json:"to,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Error     string `json:"error,omitempty"`
}

// historyPath returns the app's autoscale history file, following the same
// per-app directory convention as deploy.json.
func historyPath(app string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".phelix", "apps", app, "autoscale_history.jsonl"), nil
}

// AppendHistoryRecord appends one record. It is best-effort at the call
// site; the error is returned for tests and diagnostics only.
func AppendHistoryRecord(app string, rec HistoryRecord) error {
	if rec.Time == "" {
		rec.Time = time.Now().UTC().Format(time.RFC3339)
	}
	if rec.App == "" {
		rec.App = app
	}
	path, err := historyPath(app)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// ReadHistory returns the app's history, oldest first, capped to the most
// recent limit records (0 means all). Malformed lines are skipped.
func ReadHistory(app string, limit int) ([]HistoryRecord, error) {
	path, err := historyPath(app)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var records []HistoryRecord
	start := 0
	for i := 0; i <= len(data); i++ {
		if i == len(data) || data[i] == '\n' {
			if i > start {
				var rec HistoryRecord
				if json.Unmarshal(data[start:i], &rec) == nil && rec.Kind != "" {
					records = append(records, rec)
				}
			}
			start = i + 1
		}
	}
	if limit > 0 && len(records) > limit {
		return records[len(records)-limit:], nil
	}
	return records, nil
}
