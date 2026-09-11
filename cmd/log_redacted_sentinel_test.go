package cmd

// log_redacted_sentinel_test.go covers H2 (docs/CLI_CHANGES_REQUIRED.md §4):
// the backend now redacts credential shapes from ingested log lines before
// storage, so any log history the CLI reads back may contain
// "[REDACTED*]"-style sentinels even for lines the CLI itself did not
// redact. The CLI's log-history read path (phelix log) must treat them as
// normal opaque text — passed through verbatim, never parsed, never treated
// as an error condition.
//
// Note: the CLI currently has no REMOTE log-history read path (phelix log
// tails local files only); this test pins the local history reader so the
// invariant holds when a remote reader is ever added on top of it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// TestGetLastNLines_PassesRedactedSentinelsThrough feeds the local
// log-history reader a file containing backend-style redaction sentinels and
// asserts they come back verbatim, without error and without any attempt to
// parse or transform them.
func TestGetLastNLines_PassesRedactedSentinelsThrough(t *testing.T) {
	sentinelLines := []string{
		"2026/09/11 10:00:01 [INFO] [app] connected to database",
		"2026/09/11 10:00:02 [ERROR] [app] db connect failed: [REDACTED:password] for user admin",
		"2026/09/11 10:00:03 [ERROR] [app] token rejected: [REDACTED:bearer]",
		"2026/09/11 10:00:04 [WARN] [app] key loaded: [REDACTED]",
		"2026/09/11 10:00:05 [INFO] [app] url: postgres://admin:[REDACTED]@db.internal:5432/prod",
		"2026/09/11 10:00:06 [INFO] [app] aws creds: [REDACTED:aws_key]",
	}

	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte(strings.Join(sentinelLines, "\n")+"\n"), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log file: %v", err)
	}
	defer f.Close()

	got, err := getLastNLines(f, len(sentinelLines))
	if err != nil {
		t.Fatalf("getLastNLines must not error on redaction sentinels: %v", err)
	}
	if len(got) != len(sentinelLines) {
		t.Fatalf("got %d lines, want %d", len(got), len(sentinelLines))
	}
	for i, want := range sentinelLines {
		if got[i] != want {
			t.Errorf("line %d mutated by the history reader:\n got: %q\nwant: %q", i, got[i], want)
		}
	}
}

// TestDisplayHistoricalLogs_RendersSentinelsAsOpaqueText runs the full
// display path (the one `phelix log` uses before tailing) against a sentinel
// file and asserts the sentinels reach stdout exactly as stored — no error,
// no un-redaction attempt, no swallowing.
func TestDisplayHistoricalLogs_RendersSentinelsAsOpaqueText(t *testing.T) {
	line := "2026/09/11 10:00:02 [ERROR] [app] secret was [REDACTED:password] and [REDACTED]"

	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte(line+"\n"), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log file: %v", err)
	}
	defer f.Close()

	out := captureStdout(t, func() {
		if err := displayHistoricalLogs(f); err != nil {
			t.Errorf("displayHistoricalLogs must not error on sentinels: %v", err)
		}
	})

	if !strings.Contains(out, "[REDACTED:password]") || !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("redaction sentinels must be rendered as normal text, got:\n%s", out)
	}
	if strings.Contains(out, "hunter2") || strings.Contains(out, "password=secret") {
		t.Fatalf("history reader unexpectedly transformed content:\n%s", out)
	}
}

// TestRedact_HandlesBackendSentinelsSafely pins the client-side redaction
// chokepoint (phelixerr.Redact, applied to every log line before it is sent
// to the backend) against backend-style sentinels: a line that already
// carries "[REDACTED...]" text is treated as ordinary content — no error, no
// un-redaction. Sentinels standing alone are preserved verbatim; one that
// happens to occupy a credential position (e.g. the password slot of a
// connection-string URL) may be re-masked, which is equally safe.
func TestRedact_HandlesBackendSentinelsSafely(t *testing.T) {
	verbatim := []string{
		"db connect failed: [REDACTED:password] for user admin",
		"token rejected: [REDACTED:bearer]",
		"key: [REDACTED]",
		"plain operational line: listening on :8080",
	}
	for _, in := range verbatim {
		if got := phelixerr.Redact(in); got != in {
			t.Errorf("Redact must leave standalone sentinel text untouched:\n in: %q\ngot: %q", in, got)
		}
	}

	// A sentinel in the URL password slot is re-masked — acceptable as long
	// as the result stays masked and the line stays useful.
	in := "postgres://admin:[REDACTED]@db.internal:5432/prod"
	got := phelixerr.Redact(in)
	if !strings.Contains(got, "***") {
		t.Errorf("re-masked URL line should carry a mask marker, got %q", got)
	}
	if !strings.Contains(got, "postgres://admin:") || !strings.Contains(got, "@db.internal:5432/prod") {
		t.Errorf("re-masked URL line should keep scheme/user/host, got %q", got)
	}
	if strings.Contains(got, "[REDACTED]") && got != in {
		t.Errorf("unexpected partial transformation: %q", got)
	}
}
