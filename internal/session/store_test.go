package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestBoundsRejectOverLongText(t *testing.T) {
	isolate(t)
	if _, err := Create(CreateOpts{Title: strings.Repeat("a", MaxTitleLen+1)}); phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
		t.Fatalf("over-long title = %v, want INVALID_ARGUMENT", phelixerr.CodeOf(err))
	}
	s := mustCreate(t, CreateOpts{})
	if _, err := Checkpoint(s.SessionID, CheckpointOpts{Note: strings.Repeat("b", MaxNoteLen+1), ExpectRev: -1}); phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
		t.Fatalf("over-long note = %v, want INVALID_ARGUMENT", phelixerr.CodeOf(err))
	}
}

func TestRedactionBeforePersistence(t *testing.T) {
	isolate(t)
	const secret = "sk-livesecretABCDEF0123456789"
	s := mustCreate(t, CreateOpts{Title: "deploy " + secret})
	if strings.Contains(s.Title, secret) {
		t.Fatalf("returned title still contains the secret: %q", s.Title)
	}
	s2, err := Checkpoint(s.SessionID, CheckpointOpts{Note: "token=" + secret, ExpectRev: -1})
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(Dir(), s2.SessionID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("secret persisted to disk: %s", raw)
	}
}

func TestListOrderingFilterTruncation(t *testing.T) {
	isolate(t)
	var ids []string
	for i := 0; i < 3; i++ {
		s := mustCreate(t, CreateOpts{App: "billing"})
		ids = append(ids, s.SessionID)
		time.Sleep(2 * time.Millisecond)
	}
	other := mustCreate(t, CreateOpts{App: "web"})
	if _, err := Complete(ids[0], CompleteOpts{ExpectRev: -1}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	all, _, trunc, err := List("", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 || trunc {
		t.Fatalf("List all: n=%d trunc=%v, want 4/false", len(all), trunc)
	}
	if all[0].SessionID != other.SessionID {
		t.Fatalf("ordering: first=%s, want newest %s", all[0].SessionID, other.SessionID)
	}
	active, _, _, _ := List(StatusActive, "", 0)
	for _, s := range active {
		if s.Status != StatusActive {
			t.Fatalf("status filter leaked a %q session", s.Status)
		}
	}
	web, _, _, _ := List("", "web", 0)
	if len(web) != 1 || web[0].App != "web" {
		t.Fatalf("app filter: n=%d, want exactly 1 web session", len(web))
	}
	lim, _, trunc2, _ := List("", "", 2)
	if len(lim) != 2 || !trunc2 {
		t.Fatalf("limit 2: n=%d trunc=%v, want 2/true", len(lim), trunc2)
	}
}

func TestOptimisticConcurrencyConflict(t *testing.T) {
	isolate(t)
	s := mustCreate(t, CreateOpts{}) // rev 1
	s2, err := Checkpoint(s.SessionID, CheckpointOpts{Note: "a", ExpectRev: 1})
	if err != nil {
		t.Fatalf("checkpoint at rev 1: %v", err)
	}
	if s2.Rev != 2 {
		t.Fatalf("rev after checkpoint = %d, want 2", s2.Rev)
	}
	_, err = Checkpoint(s.SessionID, CheckpointOpts{Note: "b", ExpectRev: 1})
	if phelixerr.CodeOf(err) != phelixerr.CodeSessionConflict {
		t.Fatalf("stale expected-rev = %v, want SESSION_CONFLICT", phelixerr.CodeOf(err))
	}
	if !phelixerr.Retryable(phelixerr.CodeSessionConflict) {
		t.Fatal("SESSION_CONFLICT must be retryable")
	}
	got, _ := Load(s.SessionID)
	if got.Rev != 2 {
		t.Fatalf("conflict must not mutate: rev=%d, want 2", got.Rev)
	}
}

func TestEventCapRefusesCheckpointButAllowsTerminal(t *testing.T) {
	isolate(t)
	s := mustCreate(t, CreateOpts{})
	full, _ := Load(s.SessionID)
	for len(full.Events) < MaxEvents {
		full.appendEvent(EventCheckpoint, "x")
	}
	data, err := encodeSession(full)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := sessionPath(full.SessionID)
	if err := atomicReplace(p, data); err != nil {
		t.Fatal(err)
	}
	if _, err := Checkpoint(full.SessionID, CheckpointOpts{Note: "more", ExpectRev: -1}); phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
		t.Fatalf("checkpoint at event cap = %v, want INVALID_ARGUMENT", phelixerr.CodeOf(err))
	}
	if _, err := Complete(full.SessionID, CompleteOpts{Result: "closing at cap", ExpectRev: -1}); err != nil {
		t.Fatalf("terminal transition at event cap must still be allowed: %v", err)
	}
}
