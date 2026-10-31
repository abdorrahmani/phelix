package logs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDetectAppRecord_SlogJSON(t *testing.T) {
	line := `{"time":"2026-10-29T18:00:00.123Z","level":"WARN","msg":"disk almost full","app":"orders"}`
	rec, ok := detectAppRecord(line, false, time.Now())
	if !ok {
		t.Fatalf("slog JSON line did not parse")
	}
	if rec.Level != LevelWarning {
		t.Errorf("level = %q, want warn", rec.Level)
	}
	if rec.Msg != "disk almost full" {
		t.Errorf("msg = %q", rec.Msg)
	}
	if got := ParseRecordTime(rec.TS).Format(RFC3339Milli); got != "2026-10-29T18:00:00.123Z" {
		t.Errorf("ts = %q", rec.TS)
	}
	if rec.Fields["app"] != "orders" {
		t.Errorf("extra keys must land in fields, got %v", rec.Fields)
	}
	if rec.Stream != StreamUnknown {
		t.Errorf("JSON lines carry no stream, got %q", rec.Stream)
	}
}

func TestDetectAppRecord_TracingJSON(t *testing.T) {
	line := `{"timestamp":"2026-10-29T18:00:00.123456Z","level":"ERROR","fields":{"message":"boom","request_id":"req_7"},"target":"gateway"}`
	rec, ok := detectAppRecord(line, false, time.Now())
	if !ok {
		t.Fatalf("tracing JSON line did not parse")
	}
	if rec.Level != LevelError || rec.Msg != "boom" {
		t.Errorf("level/msg = %q/%q, want error/boom", rec.Level, rec.Msg)
	}
	if rec.RequestID != "req_7" {
		t.Errorf("nested request_id = %q, want req_7", rec.RequestID)
	}
	if rec.Fields["target"] != "gateway" {
		t.Errorf("fields = %v", rec.Fields)
	}
}

func TestDetectAppRecord_TextLevels(t *testing.T) {
	cases := []struct {
		line  string
		level LogLevel
		msg   string
	}{
		{"level=warn user quota nearly exhausted", LevelWarning, "level=warn user quota nearly exhausted"},
		{"[WARN] slow upstream", LevelWarning, "[WARN] slow upstream"},
		{"ERROR: bad thing", LevelError, "ERROR: bad thing"},
		{"panic: runtime error", LevelError, "panic: runtime error"},
		{"just some plain output", LevelInfo, "just some plain output"},
		{"2026/08/19 14:22:31 [error] [stderr] marker wins", LevelError, "marker wins"},
	}
	for _, c := range cases {
		rec, ok := detectAppRecord(c.line, false, time.Now())
		if !ok {
			t.Fatalf("%q did not parse", c.line)
		}
		if rec.Level != c.level {
			t.Errorf("%q level = %q, want %q", c.line, rec.Level, c.level)
		}
		if rec.Msg != c.msg {
			t.Errorf("%q msg = %q, want %q", c.line, rec.Msg, c.msg)
		}
	}
}

func TestDetectAppRecord_TextTimestampKept(t *testing.T) {
	rec, ok := detectAppRecord("2026/08/19 14:22:31 WARN disk tight", false, time.Now())
	if !ok {
		t.Fatalf("line did not parse")
	}
	if got := ParseRecordTime(rec.TS).Format(timeLayout); got != "2026/08/19 14:22:31" {
		t.Errorf("leading timestamp lost: ts=%q", rec.TS)
	}
	if rec.Msg != "WARN disk tight" {
		t.Errorf("msg = %q", rec.Msg)
	}
}

func TestDetectAppRecord_JSONPayloadIsNotALogRecord(t *testing.T) {
	line := `{"user":{"id":7},"action":"buy"}`
	rec, ok := detectAppRecord(line, false, time.Now())
	if !ok {
		t.Fatalf("line must not be dropped")
	}
	if rec.Msg != line {
		t.Errorf("arbitrary JSON payload must stay raw text, got %q", rec.Msg)
	}
	if rec.Level != LevelInfo {
		t.Errorf("arbitrary JSON payload must default to info, got %q", rec.Level)
	}
}

func TestDetectAppRecord_JSONModeForced(t *testing.T) {
	rec, ok := detectAppRecord("plain text under a json-only app", true, time.Now())
	if !ok {
		t.Fatalf("forced JSON mode must still yield a record")
	}
	if rec.Level != LevelInfo || rec.Msg != "plain text under a json-only app" {
		t.Errorf("forced JSON fallback: %+v", rec)
	}
}

func TestDetectAppRecord_Blank(t *testing.T) {
	if _, ok := detectAppRecord("   ", false, time.Now()); ok {
		t.Errorf("blank line must not produce a record")
	}
}

func TestCollectAppLogs_LogLevelFilter(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)

	target := AppLogTarget{ID: "apps_filter", Name: "orders", LogLevel: "warn"}
	logFile := AppLogPath(target.ID)
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "an informational line\nERROR: a bad line\nlevel=warn a warned line\n"
	if err := os.WriteFile(logFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := CollectAppLogs([]AppLogTarget{target}, "srv-1")
	if err != nil {
		t.Fatalf("CollectAppLogs returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 after the info line is filtered", len(got))
	}
	for _, e := range got {
		if e.Level == LevelInfo {
			t.Errorf("info line survived the warn filter: %+v", e)
		}
	}
}
