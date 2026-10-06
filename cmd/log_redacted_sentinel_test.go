package cmd

// log_redacted_sentinel_test.go covers H2 (docs/CLI_CHANGES_REQUIRED.md §4):
// the backend now redacts credential shapes from ingested log lines before
// storage, so any log history the CLI reads back may contain
// "[REDACTED*]"-style sentinels even for lines the CLI itself did not
// redact. The CLI's log-history read path (phelix log) must treat them as
// normal opaque text — never parsed, never treated as an error condition.
//
// Since the Phase 1 machine contract, the display path additionally passes
// every line through the centralized redactor (phelixerr.Redact): managed
// apps can print anything, so display must not become a secret-leak path.
// Standalone sentinels are preserved verbatim by Redact (pinned by
// TestRedact_HandlesBackendSentinelsSafely); a sentinel occupying a
// credential slot (e.g. a URL password) may be re-masked, which is equally
// safe.
//
// Note: the CLI has no REMOTE log-history read path (phelix log reads local
// files only); these tests pin the local history reader and display path.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// TestReadLogHistory_PassesRedactedSentinelsThrough feeds the local
// log-history reader a file containing backend-style redaction sentinels and
// asserts they come back verbatim, without error and without any attempt to
// parse or transform them.
func TestReadLogHistory_PassesRedactedSentinelsThrough(t *testing.T) {
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

	got, total, err := readLogHistory(f, len(sentinelLines), nil)
	if err != nil {
		t.Fatalf("readLogHistory must not error on redaction sentinels: %v", err)
	}
	if len(got) != len(sentinelLines) {
		t.Fatalf("got %d lines, want %d", len(got), len(sentinelLines))
	}
	if total != len(sentinelLines) {
		t.Fatalf("total = %d, want %d", total, len(sentinelLines))
	}
	for i, want := range sentinelLines {
		if got[i] != want {
			t.Errorf("line %d mutated by the history reader:\n got: %q\nwant: %q", i, got[i], want)
		}
	}
}

// TestShowLogView_RendersSentinelsAsOpaqueText runs the full display path
// (the one `phelix log` uses before tailing) against a sentinel file and
// asserts the sentinels survive as normal redacted text — no error, no
// un-redaction attempt, no swallowing, no new secret exposure.
func TestShowLogView_RendersSentinelsAsOpaqueText(t *testing.T) {
	line := "2026/09/11 10:00:02 [ERROR] [app] secret was [REDACTED:password] and [REDACTED]"

	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte(line+"\n"), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	prevNoFollow := logNoFollow
	logNoFollow = true
	defer func() { logNoFollow = prevNoFollow }()

	out := captureStdout(t, func() {
		if err := showLogView(logViewSpec{Source: "app", Name: "testapp", Path: path}, nil); err != nil {
			t.Errorf("showLogView must not error on sentinels: %v", err)
		}
	})

	if !strings.Contains(out, "[REDACTED:password]") || !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("redaction sentinels must be rendered as normal text, got:\n%s", out)
	}
	if strings.Contains(out, "hunter2") || strings.Contains(out, "password=secret") {
		t.Fatalf("history reader unexpectedly transformed content:\n%s", out)
	}
}

// TestShowLogView_RedactsCredentialShapedLines pins the display-path
// redaction: a raw credential that the log file itself contains (an app
// printing its env, for instance) must be masked before it reaches the
// terminal or a JSON consumer.
func TestShowLogView_RedactsCredentialShapedLines(t *testing.T) {
	lines := []string{
		"2026/09/11 10:00:01 [INFO] [app] starting with password=supersecret123",
		"2026/09/11 10:00:02 [INFO] [app] using token ghp_abcdef0123456789012345678901234567890",
		"2026/09/11 10:00:03 [INFO] [app] ready on :8080",
	}

	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	prevNoFollow := logNoFollow
	logNoFollow = true
	defer func() { logNoFollow = prevNoFollow }()

	out := captureStdout(t, func() {
		if err := showLogView(logViewSpec{Source: "app", Name: "testapp", Path: path}, nil); err != nil {
			t.Errorf("showLogView: %v", err)
		}
	})

	if strings.Contains(out, "supersecret123") || strings.Contains(out, "ghp_abcdef") {
		t.Fatalf("credential-shaped log content must be redacted on display, got:\n%s", out)
	}
	if !strings.Contains(out, "ready on :8080") {
		t.Fatalf("benign log content must survive redaction, got:\n%s", out)
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
