package deploy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/health"
)

// Tests for the structured rollback history: recorded at the terminal point of
// the rollback transaction, for both outcomes, with the mode actually used.

func recordPath(app string) string {
	dir, _ := appDataDir(app)
	return filepath.Join(dir, "rollback_history.jsonl")
}

func TestRecordRollbackResultSuccess(t *testing.T) {
	resetHome(t)
	RecordRollbackResult("hist-app", 15, 14, "blue-green", "", nil, nil)

	data, err := os.ReadFile(recordPath("hist-app"))
	if err != nil {
		t.Fatalf("history file not written: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly 1 record, got %d", len(lines))
	}
	var rec RollbackHistoryRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("record is not valid JSON: %v\nline: %s", err, lines[0])
	}
	if rec.From != "v15" || rec.To != "v14" || rec.Status != RollbackStatusSuccess || rec.Mode != "blue-green" {
		t.Errorf("record = %+v", rec)
	}
	if rec.Time.IsZero() {
		t.Errorf("record time not set")
	}
}

func TestRecordRollbackResultFailure(t *testing.T) {
	resetHome(t)
	RecordRollbackResult("hist-app", 15, 14, "rolling", "", nil, context.DeadlineExceeded)

	recs, skipped, err := ReadRollbackHistory("hist-app")
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Status != RollbackStatusFailed {
		t.Errorf("a rollback that failed must be recorded FAILED, got %q", rec.Status)
	}
	if !strings.Contains(rec.Error, "context deadline exceeded") {
		t.Errorf("error not recorded: %q", rec.Error)
	}
	if rec.Mode != "rolling" {
		t.Errorf("mode = %q, want rolling", rec.Mode)
	}
}

func TestReadRollbackHistoryMissingFileIsEmpty(t *testing.T) {
	resetHome(t)
	recs, skipped, err := ReadRollbackHistory("never-rolled-back")
	if err != nil {
		t.Fatalf("missing history must not error: %v", err)
	}
	if len(recs) != 0 || skipped != 0 {
		t.Errorf("recs=%d skipped=%d, want empty", len(recs), skipped)
	}
}

func TestReadRollbackHistorySkipsMalformedLines(t *testing.T) {
	resetHome(t)
	path := recordPath("hist-app")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	good, _ := json.Marshal(RollbackHistoryRecord{
		Time: time.Now(), App: "hist-app", From: "v3", To: "v2",
		Status: RollbackStatusSuccess, Mode: "classic",
	})
	content := string(good) + "\n" +
		"this is not json\n" +
		"{\"truncated\": true\n" +
		"{}\n" + // valid JSON but zero time → malformed record
		"2026-01-01T00:00:00Z rollback v2 -> v1 ok\n" + // legacy text line
		"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, skipped, err := ReadRollbackHistory("hist-app")
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 4 {
		t.Errorf("skipped = %d, want 4", skipped)
	}
	if len(recs) != 1 || recs[0].To != "v2" {
		t.Errorf("records = %+v, want only the valid one", recs)
	}
}

// TestExecuteRollback_RecordsSuccessHistory: exactly one history record for a
// genuinely completed rollback, with the mode of the deployment that ran it.
func TestExecuteRollback_RecordsSuccessHistory(t *testing.T) {
	resetHome(t)
	app := "hist-rolling-ok"
	rollbackFixture(t, app, 2, 999999, 40000)
	fl := &httpLauncher{}
	defer fl.close()

	err := ExecuteRollback(context.Background(), RollbackOptions{
		AppName:       app,
		TargetVersion: 1,
		Replicas:      2,
		Launcher:      fl.Launch,
		ProxyClient:   &fakeProxyClient{alive: true},
		HealthProvider: func(string) *health.DeployTierConfig {
			return fastHealth()
		},
		Logger: &fakeLogger{},
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}

	recs, _, err := ReadRollbackHistory(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want exactly 1 history record, got %d: %+v", len(recs), recs)
	}
	rec := recs[0]
	if rec.Status != RollbackStatusSuccess || rec.From != "v2" || rec.To != "v1" || rec.Mode != string(ModeRolling) {
		t.Errorf("record = %+v, want success 2 -> 1 via rolling", rec)
	}
}

// TestExecuteRollback_RecordsFailedHistory: a rollback that starts but fails
// mid-transaction must be recorded FAILED, never SUCCESS.
func TestExecuteRollback_RecordsFailedHistory(t *testing.T) {
	resetHome(t)
	app := "hist-rolling-fail"
	rollbackFixture(t, app, 2, 999999, 40000)

	err := ExecuteRollback(context.Background(), RollbackOptions{
		AppName:       app,
		TargetVersion: 1,
		Replicas:      2,
		Launcher:      closedLauncher{}.Launch,
		ProxyClient:   &fakeProxyClient{alive: true},
		HealthProvider: func(string) *health.DeployTierConfig {
			return fastHealthShortTimeout()
		},
		Logger: &fakeLogger{},
	})
	if err == nil {
		t.Fatal("expected rollback to fail with unhealthy target")
	}

	recs, _, err := ReadRollbackHistory(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want exactly 1 history record for the failed attempt, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Status != RollbackStatusFailed {
		t.Errorf("status = %q, want failed", rec.Status)
	}
	if rec.Mode != string(ModeRolling) {
		t.Errorf("mode = %q, want rolling (the strategy the failed rollback used)", rec.Mode)
	}
	if rec.Error == "" {
		t.Errorf("failure reason must be recorded")
	}
}
