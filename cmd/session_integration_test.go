package cmd

// session_integration_test.go — the real-binary end-to-end session acceptance.
// It runs the actual `phelix session …` CLI as a subprocess against an isolated
// $HOME and drives the lifecycle over --json: create → read → list → context →
// checkpoint → recover-in-a-fresh-process → complete → refused invalid
// transition. Because every invocation is a brand-new process, the recovery
// step is literally read-based recovery across a restart, and it executes
// nothing (a session has no execution path at all).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type itEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	Status        string          `json:"status"`
	OperationID   string          `json:"operation_id"`
	Result        json.RawMessage `json:"result"`
	Error         *struct {
		Code     string `json:"code"`
		ExitCode int    `json:"exit_code"`
	} `json:"error"`
}

// parseEnv asserts stdout is exactly one machine envelope (any trailing
// non-whitespace makes json.Unmarshal fail — so this also pins stdout purity).
func parseEnv(t *testing.T, stdout string) itEnvelope {
	t.Helper()
	var env itEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not a single machine envelope: %v\nstdout=%q", err, stdout)
	}
	if env.SchemaVersion != "1" {
		t.Fatalf("schema_version = %q, want 1", env.SchemaVersion)
	}
	return env
}

func parseSession(t *testing.T, raw json.RawMessage) (id, status string, rev int, actor string) {
	t.Helper()
	var s struct {
		SessionID string `json:"session_id"`
		Status    string `json:"status"`
		Rev       int    `json:"rev"`
		Actor     struct {
			Type string `json:"type"`
		} `json:"actor"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode session result: %v", err)
	}
	return s.SessionID, s.Status, s.Rev, s.Actor.Type
}

func TestCLISession_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and spawns the real binary; skipped under -short")
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".phelix", "logs"), 0o755); err != nil {
		t.Fatalf("mkdir .phelix: %v", err)
	}
	run := func(args ...string) (int, string, string) { return runPhelixInHome(t, home, args...) }

	// 1. Create — read-shaped envelope (no operation_id), active, cli provenance.
	code, out, errout := run("session", "create", "--title", "ship", "--app", "billing", "--json")
	if code != 0 {
		t.Fatalf("create exit=%d stderr=%q", code, errout)
	}
	env := parseEnv(t, out)
	if env.OperationID != "" {
		t.Fatalf("session envelope must carry no operation_id, got %q", env.OperationID)
	}
	sid, status, _, actor := parseSession(t, env.Result)
	if status != "active" || actor != "cli" {
		t.Fatalf("created session status=%q actor=%q, want active/cli", status, actor)
	}

	// 2–4. Read, list and the current context all succeed over --json.
	for _, args := range [][]string{{"session", "show", sid, "--json"}, {"session", "list", "--json"}, {"context", "--json"}} {
		code, out, errout = run(args...)
		if code != 0 || parseEnv(t, out).Status != "succeeded" {
			t.Fatalf("%v exit=%d stderr=%q", args, code, errout)
		}
	}

	// 5. Checkpoint advances the revision.
	code, out, errout = run("session", "checkpoint", sid, "--note", "halfway", "--step", "build", "--json")
	if code != 0 {
		t.Fatalf("checkpoint exit=%d stderr=%q", code, errout)
	}
	if _, _, rev, _ := parseSession(t, parseEnv(t, out).Result); rev != 2 {
		t.Fatalf("rev after checkpoint = %d, want 2", rev)
	}

	// 6. Recovery in a FRESH process: the checkpoint persisted and the session
	//    did not auto-advance — reading it changes nothing.
	code, out, _ = run("session", "show", sid, "--json")
	if _, st, rev, _ := parseSession(t, parseEnv(t, out).Result); st != "active" || rev != 2 {
		t.Fatalf("recovered session status=%q rev=%d, want active/2", st, rev)
	}

	// 7. Complete.
	code, out, errout = run("session", "complete", sid, "--result", "done", "--json")
	if code != 0 {
		t.Fatalf("complete exit=%d stderr=%q", code, errout)
	}
	if _, st, _, _ := parseSession(t, parseEnv(t, out).Result); st != "completed" {
		t.Fatalf("status after complete = %q, want completed", st)
	}

	// 8. A transition on a terminal session is refused: exit 3, JSON error on
	//    stdout, human error on stderr, and the record is unchanged.
	code, out, errout = run("session", "checkpoint", sid, "--note", "x", "--json")
	if code != ExitValidation {
		t.Fatalf("terminal checkpoint exit=%d, want %d", code, ExitValidation)
	}
	if e := parseEnv(t, out).Error; e == nil || e.Code != "SESSION_INVALID_TRANSITION" {
		t.Fatalf("terminal checkpoint envelope error = %+v, want SESSION_INVALID_TRANSITION", e)
	}
	if !strings.Contains(errout, "SESSION_INVALID_TRANSITION") {
		t.Fatalf("human error block missing from stderr: %q", errout)
	}
}
