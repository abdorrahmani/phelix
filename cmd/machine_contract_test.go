package cmd

// machine_contract_test.go pins the Phase 1 machine contract at the command
// layer: pure JSON stdout, versioned envelopes, structured error rendering,
// and bounded log retrieval. captureStdout swaps os.Stdout via a pipe;
// commands in machine mode pin their envelope writer to the (swapped) stdout
// and detour progress to stderr, so everything on the captured pipe must be
// exactly one JSON envelope.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
)

// decodeEnvelope asserts out is exactly one machine envelope with the current
// schema_version and decodes it.
func decodeEnvelope(t *testing.T, out string) *machine.Envelope {
	t.Helper()
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		t.Fatal("machine mode produced no stdout output")
	}
	var env machine.Envelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		t.Fatalf("stdout is not a single JSON envelope: %v\nstdout=%q", err, trimmed)
	}
	if env.SchemaVersion != machine.SchemaVersion {
		t.Fatalf("schema_version = %q, want %q", env.SchemaVersion, machine.SchemaVersion)
	}
	return &env
}

func TestDoctorJSON_PureStdoutEnvelope(t *testing.T) {
	dir := t.TempDir()
	// A minimal passing project so the doctor SUCCESS path runs (writes the
	// result envelope itself); the failure path is pinned separately below.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainSrc := "package main\n\nimport (\"os\"\n)\n\nfunc main() { _ = os.Getenv(\"PORT\") }\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	prevWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(prevWd) }()

	doctorJSONPrev := doctorJSON
	doctorJSON = true
	defer func() { doctorJSON = doctorJSONPrev }()

	// doctorJSON=true makes the command enter machine mode itself — the test
	// must not double-enter (the second pin would target stderr).
	defer machine.LeaveJSON()
	out := captureStdout(t, func() {
		if err := DoctorCmd.RunE(DoctorCmd, nil); err != nil {
			t.Errorf("doctor on a valid project must succeed: %v", err)
		}
	})

	env := decodeEnvelope(t, out)
	raw, _ := json.Marshal(env.Result)
	var result doctorJSONResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("result is not a doctorJSONResult: %v (%s)", err, raw)
	}
	if len(result.Checks) == 0 {
		t.Fatal("doctor result must carry its checks")
	}
	for _, c := range result.Checks {
		switch c.Status {
		case machine.StatusSucceeded, machine.StatusPending, machine.StatusFailed:
		default:
			t.Fatalf("check %q status %q is outside the lifecycle vocabulary", c.Name, c.Status)
		}
	}
}

func TestDoctorJSON_Exits3OnFailingChecks(t *testing.T) {
	dir := t.TempDir()
	prevWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(prevWd) }()

	err := DoctorCmd.RunE(DoctorCmd, nil)
	if !phelixerr.IsCode(err, phelixerr.CodeValidation) {
		t.Fatalf("doctor on an empty dir must return VALIDATION_ERROR, got %v", err)
	}
	if got := ExitCodeFor(err); got != ExitValidation {
		t.Fatalf("doctor failure exit = %d, want %d (distinct from usage)", got, ExitValidation)
	}
}

// TestDoctorJSON_FailurePathEmitsNoStdout pins the single-document contract:
// on failure doctor writes no result envelope of its own — the error boundary
// owns the one failure envelope, and RunE must leave stdout untouched.
func TestDoctorJSON_FailurePathEmitsNoStdout(t *testing.T) {
	dir := t.TempDir()
	prevWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(prevWd) }()

	doctorJSONPrev := doctorJSON
	doctorJSON = true
	defer func() { doctorJSON = doctorJSONPrev }()
	defer machine.LeaveJSON()

	var out string
	out = captureStdout(t, func() {
		if err := DoctorCmd.RunE(DoctorCmd, nil); err == nil {
			t.Error("doctor on an empty dir must fail")
		}
	})
	if strings.TrimSpace(out) != "" {
		t.Fatalf("failing doctor must leave stdout empty for the boundary's error envelope, got:\n%s", out)
	}
}

func TestRenderError_MachineModeWritesSingleErrorEnvelope(t *testing.T) {
	prevErrOut := errOut
	prevStdout := os.Stdout

	// The pipe becomes "the real stdout" BEFORE EnterJSON pins the envelope
	// writer, so the envelope lands on the captured pipe. The human block
	// goes to errOut — discarded here; the pipe must carry JSON only.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	restore := machine.EnterJSON()
	errOut = io.Discard
	machine.SetActiveOperation("op_0123456789abcdef")
	defer machine.LeaveJSON()
	defer func() {
		restore()
		errOut = prevErrOut
		os.Stdout = prevStdout
	}()

	exit := renderCLIError(phelixerr.New(phelixerr.CodeDeployFailed, "deploy exploded with password=hunter2"), false)
	_ = w.Close()
	buf, _ := io.ReadAll(r)
	_ = r.Close()

	if exit != ExitDeploy {
		t.Fatalf("exit = %d, want %d (--json must not change semantics)", exit, ExitDeploy)
	}
	env := decodeEnvelope(t, string(buf))
	if env.Status != machine.StatusFailed {
		t.Fatalf("status = %q", env.Status)
	}
	if env.Error == nil || env.Error.Code != "DEPLOY_FAILED" || env.Error.ExitCode != ExitDeploy {
		t.Fatalf("error body = %+v", env.Error)
	}
	if env.OperationID != "op_0123456789abcdef" {
		t.Fatalf("operation-associated errors must carry the registered operation id, got %q", env.OperationID)
	}
	if strings.Contains(env.Error.Message, "hunter2") {
		t.Fatalf("machine error envelope leaked a secret: %q", env.Error.Message)
	}
}

func TestLogJSON_BoundedSnapshot(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, "2026/10/06 10:00:00 [INFO] [app] line "+strings.Repeat("x", 10))
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logJSONPrev, linesPrev := logJSON, logLines
	defer func() { logJSON, logLines = logJSONPrev, linesPrev }()
	defer machine.LeaveJSON()
	logJSON, logLines = true, 5

	out := captureStdout(t, func() {
		restore := machine.EnterJSON()
		defer restore()
		if err := showLogView(logViewSpec{Source: "app", Name: "demo", Path: logPath}, nil); err != nil {
			t.Errorf("showLogView: %v", err)
		}
	})

	env := decodeEnvelope(t, out)
	raw, _ := json.Marshal(env.Result)
	var result logResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("result is not a logResult: %v (%s)", err, raw)
	}
	if result.Count != 5 || len(result.Items) != 5 {
		t.Fatalf("bounded snapshot returned %d items (count %d), want 5", len(result.Items), result.Count)
	}
	if !result.Truncated {
		t.Fatal("50-line file read at 5 must report truncated")
	}
}

func TestLogJSON_SinceFilterDropsOldLines(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	content := "2026/10/06 09:00:00 [INFO] [app] old\n2026/10/06 09:30:00 [INFO] [app] new\n"
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	prevJSON, prevSince := logJSON, logSince
	defer func() { logJSON, logSince = prevJSON, prevSince }()
	defer machine.LeaveJSON()
	logJSON, logSince = true, "2026-10-06T09:15:00Z"

	since, err := parseLogSince(logSince)
	if err != nil {
		t.Fatalf("parseLogSince: %v", err)
	}

	out := captureStdout(t, func() {
		restore := machine.EnterJSON()
		defer restore()
		if err := showLogView(logViewSpec{Source: "app", Name: "demo", Path: logPath}, since); err != nil {
			t.Errorf("showLogView: %v", err)
		}
	})

	env := decodeEnvelope(t, out)
	raw, _ := json.Marshal(env.Result)
	var result logResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Count != 1 || !strings.Contains(result.Items[0], "new") {
		t.Fatalf("since filter kept %d items, want only the newer line: %+v", result.Count, result.Items)
	}
}

func TestLogSince_ParsesDurationsAndTimestamps(t *testing.T) {
	if _, err := parseLogSince(""); err != nil {
		t.Fatalf("empty --since is no filter: %v", err)
	}
	d, err := parseLogSince("30m")
	if err != nil {
		t.Fatalf("duration --since: %v", err)
	}
	if time.Since(*d) < 29*time.Minute {
		t.Fatalf("duration --since must resolve backwards from now: %v", d)
	}
	if _, err := parseLogSince("2026-10-06T09:15:00Z"); err != nil {
		t.Fatalf("RFC3339 --since: %v", err)
	}
	for _, bad := range []string{"-5m", "yesterday", "0"} {
		if _, err := parseLogSince(bad); err == nil {
			t.Errorf("--since %q must be rejected", bad)
		}
	}
}

func TestLogJSON_ImpliesNonFollow(t *testing.T) {
	prevJSON, prevNoFollow := logJSON, logNoFollow
	defer func() { logJSON, logNoFollow = prevJSON, prevNoFollow }()
	logJSON, logNoFollow = true, false

	// --json must turn the snapshot into a bounded, terminating response:
	// no tailing, no hang, envelope on stdout.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	if err := os.WriteFile(logPath, []byte("2026/10/06 10:00:00 [INFO] [app] hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		restore := machine.EnterJSON()
		defer restore()
		if err := showLogView(logViewSpec{Source: "app", Name: "demo", Path: logPath}, nil); err != nil {
			t.Errorf("showLogView: %v", err)
		}
	})
	decodeEnvelope(t, out)
}

func TestMachineEnterJSON_DetoursProgress(t *testing.T) {
	defer machine.LeaveJSON()
	out := captureStdout(t, func() {
		restore := machine.EnterJSON()
		defer restore()
		// This is what every progress site does (fmt.Printf → os.Stdout):
		// in machine mode it must reach stderr, never the JSON stream.
		fmt.Printf("rebuilding application 'demo'\n")
	})
	if strings.Contains(out, "rebuilding application") {
		t.Fatalf("progress contaminated the JSON stream: %q", out)
	}
}
