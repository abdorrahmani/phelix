package deploy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// RollbackHistoryRecord is one terminal rollback outcome, persisted as a JSON
// line in the per-app rollback history file. Unlike the free-text
// rollback.log trace (which mixes per-step telemetry), this is the canonical
// structured source for `phelix rollback history`: it records both successes
// and failures, and the deployment mode actually used by that rollback.
//
// Reason and Verification are optional: records written before they existed
// (and rollbacks without them) simply omit the fields, and
// ReadRollbackHistory loads them unchanged.
type RollbackHistoryRecord struct {
	Time   time.Time `json:"time"`
	App    string    `json:"app"`
	From   string    `json:"from"`   // "v15"
	To     string    `json:"to"`     // "v14"
	Status string    `json:"status"` // "success" | "failed" — execution outcome only
	Mode   string    `json:"mode"`   // "blue-green" | "rolling" | "classic"
	Error  string    `json:"error,omitempty"`
	// Reason is the operator-supplied explanation for the rollback, when one
	// was given. JSON-serialized through encoding/json (never concatenated),
	// so the text cannot break the structured record. Size is bounded by
	// ValidateRollbackReason at the CLI boundary.
	Reason string `json:"reason,omitempty"`
	// Source distinguishes a manually requested rollback from an automatic
	// post-deployment recovery. Empty on records written before the field
	// existed (and treated as manual by readers that default it).
	Source string `json:"source,omitempty"`
	// Verification describes post-rollback stability observation. nil when
	// verification was not requested.
	Verification *RollbackVerification `json:"verification,omitempty"`
}

// Rollback history Source values.
const (
	RollbackSourceManual    = "manual"
	RollbackSourceAutomatic = "automatic"
)

// RollbackVerification records the outcome of post-rollback stability
// observation. Status distinguishes passed / failed / cancelled (the user
// interrupted the observation window after the rollback itself had already
// completed successfully).
type RollbackVerification struct {
	Requested bool      `json:"requested"`
	Duration  string    `json:"duration"` // Go duration string, e.g. "30s"
	Status    string    `json:"status"`   // passed | failed | cancelled
	Error     string    `json:"error,omitempty"`
	At        time.Time `json:"at,omitempty"`
}

// Status strings for RollbackHistoryRecord. Execution outcome only: a
// verification failure keeps Status "success" and records the failure in
// Verification — the two must never blur.
const (
	RollbackStatusSuccess = "success"
	RollbackStatusFailed  = "failed"
)

// Status strings for RollbackVerification.Status.
const (
	RollbackVerifyPassed    = "passed"
	RollbackVerifyFailed    = "failed"
	RollbackVerifyCancelled = "cancelled"
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
// "rolling" or "classic"); runErr nil means success. reason is the
// operator-supplied explanation ("" when none); verification is the
// post-rollback stability outcome (nil when not requested) — it records how
// the verification window ended while Status stays the execution outcome.
// source distinguishes manual vs automatic recovery ("" defaults to manual in
// display).
func RecordRollbackResult(appName string, fromVer, toVer int, mode, reason string, verification *RollbackVerification, runErr error) {
	RecordRollbackResultSource(appName, fromVer, toVer, mode, reason, verification, runErr, "")
}

// RecordRollbackResultSource is RecordRollbackResult with an explicit origin
// tag (RollbackSourceManual / RollbackSourceAutomatic).
func RecordRollbackResultSource(appName string, fromVer, toVer int, mode, reason string, verification *RollbackVerification, runErr error, source string) {
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
		Reason: reason,
		Source: source,
	}
	if runErr != nil {
		rec.Status = RollbackStatusFailed
		rec.Error = runErr.Error()
	}
	rec.Verification = verification
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

// MaxRollbackReasonLength bounds operator-supplied rollback reason text so
// unbounded strings can never reach the history file or table output.
const MaxRollbackReasonLength = 500

// ValidateRollbackReason normalizes a rollback reason for persistence. The
// value is trimmed; internal runs of whitespace (newlines, tabs) collapse to
// single spaces so the text can never inject lines into the JSONL history or
// the human rollback.log trace, and length is capped at
// MaxRollbackReasonLength. explicit marks that the flag was actually
// supplied: an explicitly empty reason is an error, an absent one is not.
func ValidateRollbackReason(reason string, explicit bool) (string, error) {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		if explicit {
			return "", phelixerr.New(phelixerr.CodeInvalidArgument, "rollback reason cannot be empty")
		}
		return "", nil
	}
	collapsed := strings.Join(strings.Fields(trimmed), " ")
	if runes := []rune(collapsed); len(runes) > MaxRollbackReasonLength {
		return "", phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"rollback reason too long: %d characters (max %d)", len(runes), MaxRollbackReasonLength)
	}
	return collapsed, nil
}
