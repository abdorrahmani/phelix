package logs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// parseLogLine
// ---------------------------------------------------------------------------

func TestParseLogLine_StructuredApp(t *testing.T) {
	raw := "2026/08/19 14:22:31 [ERROR] [stderr] connection refused: dial tcp 10.0.0.1:5432"
	p := parseLogLine(raw)

	if p.Level != LevelError {
		t.Errorf("level = %q, want ERROR", p.Level)
	}
	if p.Stream != StreamStderr {
		t.Errorf("stream = %q, want stderr", p.Stream)
	}
	if p.Component != "" {
		t.Errorf("component = %q, want empty for app log", p.Component)
	}
	if p.Message != "connection refused: dial tcp 10.0.0.1:5432" {
		t.Errorf("message = %q", p.Message)
	}
	if want := "2026-08-19 14:22:31"; p.Timestamp.Format("2006-01-02 15:04:05") != want {
		t.Errorf("timestamp = %v, want %s", p.Timestamp, want)
	}
}

func TestParseLogLine_StructuredSelf(t *testing.T) {
	raw := "2026/08/19 14:22:31 [WARNING] [grpc] Reconnecting in 2.053s (attempt 2)..."
	p := parseLogLine(raw)

	if p.Level != LevelWarning {
		t.Errorf("level = %q, want WARNING", p.Level)
	}
	if p.Component != "grpc" {
		t.Errorf("component = %q, want grpc", p.Component)
	}
	if p.Stream != StreamUnknown {
		t.Errorf("stream = %q, want unknown for self log", p.Stream)
	}
	if p.Message != "Reconnecting in 2.053s (attempt 2)..." {
		t.Errorf("message = %q", p.Message)
	}
}

func TestParseLogLine_NormalizesWarn(t *testing.T) {
	p := parseLogLine("2026/08/19 14:22:31 [WARN] [stdout] slow request")
	if p.Level != LevelWarning {
		t.Errorf("level = %q, want WARNING (WARN normalized)", p.Level)
	}
	if p.Stream != StreamStdout {
		t.Errorf("stream = %q, want stdout", p.Stream)
	}
}

func TestParseLogLine_StructuredStdoutIsInfo(t *testing.T) {
	p := parseLogLine("2026/08/19 14:22:31 [INFO] [stdout] GET /health 200")
	if p.Level != LevelInfo || p.Stream != StreamStdout {
		t.Errorf("got level=%q stream=%q, want INFO/stdout", p.Level, p.Stream)
	}
}

func TestParseLogLine_LegacyFallback(t *testing.T) {
	// Raw app output with no phelix-structured markers.
	p := parseLogLine("2026/08/19 14:22:31 hello world")
	if p.Level != LevelInfo {
		t.Errorf("legacy default level = %q, want INFO", p.Level)
	}
	if p.Stream != StreamUnknown || p.Component != "" {
		t.Errorf("legacy stream/component = %q/%q, want unknown/empty", p.Stream, p.Component)
	}

	// Keyword heuristic.
	if q := parseLogLine("boom: failed to start"); q.Level != LevelError {
		t.Errorf("'failed' line level = %q, want ERROR", q.Level)
	}
	if q := parseLogLine("note: this is a warning"); q.Level != LevelWarning {
		t.Errorf("'warning' line level = %q, want WARNING", q.Level)
	}
	if q := parseLogLine("debug listen on :8080"); q.Level != LevelDebug {
		t.Errorf("'debug' line level = %q, want DEBUG", q.Level)
	}
}

func TestParseLogLine_UnstructuredNoTimestamp(t *testing.T) {
	p := parseLogLine("just some bare text")
	if !p.Timestamp.IsZero() {
		t.Errorf("timestamp = %v, want zero for untimestamped line", p.Timestamp)
	}
	if p.Message != "just some bare text" {
		t.Errorf("message = %q", p.Message)
	}
}

// ---------------------------------------------------------------------------
// AppLogWriter
// ---------------------------------------------------------------------------

func TestAppLogWriter_PrefixesEveryLine(t *testing.T) {
	var buf bytes.Buffer
	w := NewAppLogWriter(&buf, StreamStdout)

	// Multi-line write: "a\nb\nc\n" → three prefixed lines.
	if _, err := w.Write([]byte("a\nb\nc\n")); err != nil {
		t.Fatal(err)
	}

	got := buf.String()
	lines := bytes.Split(bytes.TrimRight([]byte(got), "\n"), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), got)
	}
	for i, l := range lines {
		if !bytes.Contains(l, []byte("[INFO] [stdout] ")) {
			t.Errorf("line %d missing INFO/stdout prefix: %q", i, l)
		}
		if i == 0 && !bytes.Contains(l, []byte("a")) {
			t.Errorf("line 0 should contain 'a': %q", l)
		}
	}
}

func TestAppLogWriter_StderrDefaultsToError(t *testing.T) {
	var buf bytes.Buffer
	w := NewAppLogWriter(&buf, StreamStderr)
	if _, err := w.Write([]byte("boom\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !bytes.Contains([]byte(got), []byte("[ERROR] [stderr] boom")) {
		t.Errorf("stderr line not prefixed ERROR/stderr: %q", got)
	}
}

func TestAppLogWriter_HoldsPartialLineUntilNewline(t *testing.T) {
	var buf bytes.Buffer
	w := NewAppLogWriter(&buf, StreamStdout)

	if _, err := w.Write([]byte("part1")); err != nil {
		t.Fatal(err)
	}
	// No newline yet — nothing should have been emitted.
	if buf.Len() != 0 {
		t.Fatalf("partial line leaked to output: %q", buf.String())
	}

	if _, err := w.Write([]byte(" two\n")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("[INFO] [stdout] part1 two")) {
		t.Errorf("buffered fragment not joined onto complete line: %q", buf.String())
	}
}

func TestAppLogWriter_CloseFlushesPartial(t *testing.T) {
	var buf bytes.Buffer
	w := NewAppLogWriter(&buf, StreamStdout)
	if _, err := w.Write([]byte("trailing")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("trailing")) {
		t.Errorf("Close did not flush partial line: %q", buf.String())
	}
}

func TestAppLogWriter_StripsCarriageReturns(t *testing.T) {
	var buf bytes.Buffer
	w := NewAppLogWriter(&buf, StreamStdout)
	if _, err := w.Write([]byte("progress 50%\r\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); bytes.Contains([]byte(got), []byte("\r")) {
		t.Errorf("carriage return leaked into emitted line: %q", got)
	}
}

// ---------------------------------------------------------------------------
// CollectAppLogs — appends-on-success regression (the "app logs not received"
// bug: results were only appended when the collector errored).
// ---------------------------------------------------------------------------

func TestCollectAppLogs_AppendsOnSuccess(t *testing.T) {
	dir := t.TempDir()
	// Point the collector's app log path at the temp dir.
	setHome(t, dir)

	target := AppLogTarget{ID: "apps_1"}
	logFile := AppLogPath(target.ID)
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		t.Fatal(err)
	}
	// Write one structured app log line to the file. Cold-start picks up the
	// last N lines, so it must be seen on the first collect.
	line := time.Now().Format(timeLayout) + " [INFO] [stdout] hello\n"
	if err := os.WriteFile(logFile, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := CollectAppLogs([]AppLogTarget{target}, "srv-1")
	if err != nil {
		t.Fatalf("CollectAppLogs returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1 — app log lines are being dropped on success", len(got))
	}
	e := got[0]
	if e.AppID != "apps_1" || e.ServerID != "srv-1" {
		t.Errorf("entry identity: app=%q server=%q", e.AppID, e.ServerID)
	}
	if e.Log != "hello" || e.Level != LevelInfo || e.Stream != StreamStdout {
		t.Errorf("entry content: log=%q level=%q stream=%q", e.Log, e.Level, e.Stream)
	}
}

func TestCollectAppLogs_EmptyFileIsNotError(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)

	target := AppLogTarget{ID: "apps_2"}
	logFile := AppLogPath(target.ID)
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logFile, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	// Two apps: one missing file (no failure), one empty (no error).
	got, err := CollectAppLogs([]AppLogTarget{{ID: "gone"}, target}, "srv-1")
	if err != nil {
		t.Fatalf("CollectAppLogs returned error for empty/missing files: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d entries, want 0 from empty files", len(got))
	}
}

// A registered app may have emitted nothing yet, or its log file may have been
// deleted. Collecting must not error forever with ENOENT — it creates the file
// once (mkdir parents + O_CREATE), then returns no error and no entries.
func TestCollectAppLogs_CreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)

	// Neither the logs directory nor the file exists for this app.
	target := AppLogTarget{ID: "never_started"}
	logFile := AppLogPath(target.ID)
	if _, err := os.Stat(logFile); !os.IsNotExist(err) {
		t.Fatalf("precondition: log file %q should not exist, stat err=%v", logFile, err)
	}

	got, err := CollectAppLogs([]AppLogTarget{target}, "srv-1")
	if err != nil {
		t.Fatalf("CollectAppLogs returned error for missing app log: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d entries from a freshly-created empty file, want 0", len(got))
	}

	// The file must now exist on disk, so the next tick does not re-error.
	if _, err := os.Stat(logFile); err != nil {
		t.Fatalf("collect did not create missing log file %q: %v", logFile, err)
	}

	// Repeat the collect — this is the regression being fixed: the second tick
	// used to re-attempt the open and hit ENOENT every time.
	if _, err := CollectAppLogs([]AppLogTarget{target}, "srv-1"); err != nil {
		t.Fatalf("second collect returned error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GenericLogCollector — inode-change reopen (rotation / app restart)
// ---------------------------------------------------------------------------

func TestGenericLogCollector_ReopensOnInodeChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	// First collector read with coldLines>0 so an inode change later re-picks
	// the new file's lines (findOffsetForLastNLines(…, 0) starts at EOF).
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewGenericLogCollector(200, 10)
	got, err := c.Collect("k", path, identityMapper)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("first collect got %d entries, want 1", len(got))
	}

	// Simulate rotation/restart: replace the file with a NEW inode via
	// rename-recreate (writers that don't preserve inode), containing a fresh
	// line.
	newPath := filepath.Join(dir, "app.log.new")
	if err := os.WriteFile(newPath, []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newPath, path); err != nil {
		t.Fatal(err)
	}

	// The collector must notice the inode change and read the new file.
	got, err = c.Collect("k", path, identityMapper)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Log != "two" {
		t.Fatalf("after rotation got %v, want entry 'two' (reopen missed new inode)", got)
	}
}

func TestGenericLogCollector_TruncateInPlaceClampsPosition(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewGenericLogCollector(200, 0)
	if _, err := c.Collect("k", path, identityMapper); err != nil {
		t.Fatal(err)
	}

	// Same inode, truncated to empty (the agent's trimLogFile does O_TRUNC on
	// the same file). The collector must clamp its offset and not error.
	if _, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := c.Collect("k", path, identityMapper)
	if err != nil {
		t.Fatalf("collect after truncate returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v entries from truncated file, want 0", got)
	}
}

func TestGenericLogCollector_AppendsNewLinesSameInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// ColdLines=0 → first collect positions at EOF and reads nothing new yet.
	c := NewGenericLogCollector(200, 0)
	first, firstErr := c.Collect("k", path, identityMapper)
	if firstErr != nil || len(first) != 0 {
		t.Fatalf("first collect: entries=%v err=%v (want 0 entries), collector should start at EOF", first, firstErr)
	}

	// Append (same inode, larger). Only the new line should come back.
	if f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
		t.Fatal(err)
	} else if _, err := f.WriteString("two\n"); err != nil {
		t.Fatal(err)
	} else if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := c.Collect("k", path, identityMapper)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Log != "two" {
		t.Fatalf("second collect got %v, want only 'two' (offset not advanced)", got)
	}
}

func identityMapper(s string) LogEntry { return LogEntry{Log: s} }

// ---------------------------------------------------------------------------
// trimLogFile — inode preservation
// ---------------------------------------------------------------------------

func TestTrimLogFile_KeepsInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")

	// Build a file just over MaxLogSize.
	over := MaxLogSize + 1024
	var sb bytes.Buffer
	for sb.Len() < over {
		sb.WriteString("padding line ::::::::::::::::::::::::::::::::::::::::::::::::\n")
	}
	if err := os.WriteFile(path, sb.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := trimLogFile(path); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if !os.SameFile(before, after) {
		t.Fatal("trimLogFile changed the file's inode — open collectors lose the file")
	}
	if after.Size() > MaxLogSize {
		t.Fatalf("file still %d bytes, want ≤ %d", after.Size(), MaxLogSize)
	}
}

func TestTrimLogFile_DoesNotTouchSmallFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.log")
	if err := os.WriteFile(path, []byte("tiny\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	if err := trimLogFile(path); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) || after.Size() != before.Size() {
		t.Fatalf("small file was modified: size %d → %d", before.Size(), after.Size())
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// setHome temporarily repoints $HOME so AppLogPath/SelfLogPath resolve into a
// test directory, and clears PHELIX_DATA_DIR so the data-dir resolution falls
// back to the HOME-based path deterministically. t.Setenv restores them after
// the test.
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("PHELIX_DATA_DIR", "")
}
