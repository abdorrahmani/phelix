package deploy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RollbackHistoryRecord is one terminal rollback outcome, persisted as a JSON
// line in the per-app rollback history file. Unlike the free-text
// rollback.log trace (which mixes per-step telemetry), this is the canonical
// structured source for `phelix rollback history`: it records both successes
// and failures, and the deployment mode actually used by that rollback.
type RollbackHistoryRecord struct {
	Time   time.Time `json:"time"`
	App    string    `json:"app"`
	From   string    `json:"from"`   // "v15"
	To     string    `json:"to"`     // "v14"
	Status string    `json:"status"` // "success" | "failed"
	Mode   string    `json:"mode"`   // "blue-green" | "rolling" | "classic"
	Error  string    `json:"error,omitempty"`
}

// Status strings for RollbackHistoryRecord.
const (
	RollbackStatusSuccess = "success"
	RollbackStatusFailed  = "failed"
)

// rollbackHistoryPath returns ~/.phelix/apps/<AppName>/rollback_history.jsonl.
func rollbackHistoryPath(appName string) (string, error) {
	dir, err := appDataDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "rollback_history.jsonl"), nil
}

// RecordRollbackResult appends one structured history line for a finished
// rollback attempt. Best-effort and never blocks or fails the rollback: a
// history write error is silently ignored, matching the rollback.log contract.
// mode is the strategy the rollback actually ran under ("blue-green",
// "rolling" or "classic"); runErr nil means success.
func RecordRollbackResult(appName string, fromVer, toVer int, mode string, runErr error) {
	path, err := rollbackHistoryPath(appName)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	rec := RollbackHistoryRecord{
		Time:   time.Now(),
		App:    appName,
		From:   fmt.Sprintf("v%d", fromVer),
		To:     fmt.Sprintf("v%d", toVer),
		Status: RollbackStatusSuccess,
		Mode:   mode,
	}
	if runErr != nil {
		rec.Status = RollbackStatusFailed
		rec.Error = runErr.Error()
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// ReadRollbackHistory reads the per-app rollback history in the order the
// events were recorded (oldest first). Malformed lines (partial writes, older
// schema) are skipped, not fatal; the caller gets the skipped count so it can
// surface a concise warning. A missing history file is an empty history, not
// an error.
func ReadRollbackHistory(appName string) (records []RollbackHistoryRecord, skipped int, err error) {
	path, err := rollbackHistoryPath(appName)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()

	records = []RollbackHistoryRecord{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := []byte(sc.Text())
		if len(line) == 0 {
			continue
		}
		var rec RollbackHistoryRecord
		if jsonErr := json.Unmarshal(line, &rec); jsonErr != nil || rec.Time.IsZero() {
			skipped++
			continue
		}
		records = append(records, rec)
	}
	if scanErr := sc.Err(); scanErr != nil {
		return records, skipped, scanErr
	}
	return records, skipped, nil
}
