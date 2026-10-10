package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
)

// isolate points the data dir at a temp directory for the duration of a test.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
}

func mustCreate(t *testing.T, opts CreateOpts) *Session {
	t.Helper()
	if opts.Actor == nil {
		opts.Actor = machine.CLIActor()
	}
	s, err := Create(opts)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s
}

func TestNewIDAndValidate(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	if !ValidateID(id) || !strings.HasPrefix(id, IDPrefix) {
		t.Fatalf("fresh id %q is not a valid %s id", id, IDPrefix)
	}
	for _, bad := range []string{"", "ses_", "ses_xyz", "pln_0000000000000000", "ses_0000000000000000x", "../ses_0000000000000000"} {
		if ValidateID(bad) {
			t.Errorf("ValidateID(%q) = true, want false", bad)
		}
	}
}

func TestCreatePersistsActiveSession(t *testing.T) {
	isolate(t)
	s := mustCreate(t, CreateOpts{Title: "ship it", App: "billing"})
	if s.Status != StatusActive || s.Rev != 1 {
		t.Fatalf("new session status=%q rev=%d, want active/1", s.Status, s.Rev)
	}
	if len(s.Events) != 1 || s.Events[0].Type != EventCreated || s.Events[0].Seq != 0 {
		t.Fatalf("want one created event at seq 0, got %+v", s.Events)
	}
	got, err := Load(s.SessionID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SessionID != s.SessionID || got.Title != "ship it" || got.App != "billing" {
		t.Fatalf("loaded session mismatch: %+v", got)
	}
}

func TestCreateIsCreateOnly(t *testing.T) {
	isolate(t)
	s := mustCreate(t, CreateOpts{})
	// Re-persisting the same id must be refused (immutable create-only write).
	if err := insert(s); phelixerr.CodeOf(err) != phelixerr.CodeAlreadyExists {
		t.Fatalf("re-insert code = %v, want ALREADY_EXISTS", phelixerr.CodeOf(err))
	}
}

func TestDecodeFailsClosed(t *testing.T) {
	if _, err := Decode([]byte("{not json")); phelixerr.CodeOf(err) != phelixerr.CodeSessionCorrupt {
		t.Fatalf("garbage decode = %v, want SESSION_CORRUPT", phelixerr.CodeOf(err))
	}
	bad, _ := json.Marshal(&Session{SchemaVersion: "999", SessionID: "ses_0000000000000000", Status: StatusActive})
	if _, err := Decode(bad); phelixerr.CodeOf(err) != phelixerr.CodeSessionInvalid {
		t.Fatalf("wrong-version decode = %v, want SESSION_INVALID", phelixerr.CodeOf(err))
	}
	bad2, _ := json.Marshal(&Session{SchemaVersion: SchemaVersion, SessionID: "ses_0000000000000000", Status: "weird"})
	if _, err := Decode(bad2); phelixerr.CodeOf(err) != phelixerr.CodeSessionInvalid {
		t.Fatalf("unknown-status decode = %v, want SESSION_INVALID", phelixerr.CodeOf(err))
	}
}

func TestLoadMissingAndCorruptFailSafe(t *testing.T) {
	isolate(t)
	if _, err := Load("ses_0000000000000000"); phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
		t.Fatalf("missing load = %v, want NOT_FOUND", phelixerr.CodeOf(err))
	}
	if _, err := Load("not-an-id"); phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
		t.Fatalf("bad-id load = %v, want INVALID_ARGUMENT", phelixerr.CodeOf(err))
	}
	s := mustCreate(t, CreateOpts{})
	if err := os.WriteFile(filepath.Join(Dir(), s.SessionID+".json"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(s.SessionID); phelixerr.CodeOf(err) != phelixerr.CodeSessionCorrupt {
		t.Fatalf("corrupt load = %v, want SESSION_CORRUPT", phelixerr.CodeOf(err))
	}
	list, skipped, _, err := List("", "", 0)
	if err != nil {
		t.Fatalf("List over corrupt record: %v", err)
	}
	if len(list) != 0 || skipped != 1 {
		t.Fatalf("List over a corrupt record: items=%d skipped=%d, want 0/1", len(list), skipped)
	}
}

func TestTerminalSessionRejectsAllTransitions(t *testing.T) {
	isolate(t)
	s := mustCreate(t, CreateOpts{})
	if _, err := Complete(s.SessionID, CompleteOpts{Result: "done", ExpectRev: -1}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	steps := []func() error{
		func() error { _, e := Complete(s.SessionID, CompleteOpts{ExpectRev: -1}); return e },
		func() error { _, e := Fail(s.SessionID, FailOpts{ExpectRev: -1}); return e },
		func() error { _, e := Cancel(s.SessionID, CancelOpts{ExpectRev: -1}); return e },
		func() error { _, e := Checkpoint(s.SessionID, CheckpointOpts{Note: "x", ExpectRev: -1}); return e },
	}
	for i, step := range steps {
		if err := step(); phelixerr.CodeOf(err) != phelixerr.CodeSessionInvalidTransition {
			t.Fatalf("step %d on terminal session = %v, want SESSION_INVALID_TRANSITION", i, phelixerr.CodeOf(err))
		}
	}
	got, _ := Load(s.SessionID)
	if got.Status != StatusCompleted || got.Rev != 2 {
		t.Fatalf("terminal session mutated by refused transitions: status=%q rev=%d", got.Status, got.Rev)
	}
}
