package session

import (
	"fmt"
	"os"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/ops"
)

// TestPruneTerminalKeepsActiveAndNewest proves session retention keeps every
// active session and the newest-N terminal ones, pruning only older terminal
// records. (Task 5.)
func TestPruneTerminalKeepsActiveAndNewest(t *testing.T) {
	isolate(t)
	var active []string
	for i := 0; i < 3; i++ {
		active = append(active, mustCreate(t, CreateOpts{}).SessionID)
		time.Sleep(2 * time.Millisecond)
	}
	var terminal []string
	for i := 0; i < 5; i++ {
		s := mustCreate(t, CreateOpts{})
		if _, err := Complete(s.SessionID, CompleteOpts{ExpectRev: -1}); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		terminal = append(terminal, s.SessionID)
		time.Sleep(2 * time.Millisecond)
	}

	pruned, err := PruneTerminal(2)
	if err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}
	if pruned != 3 {
		t.Fatalf("pruned = %d, want 3 (5 terminal − 2 kept)", pruned)
	}
	for _, id := range active {
		if _, err := Load(id); err != nil {
			t.Fatalf("active session %s must never be pruned: %v", id, err)
		}
	}
	for _, id := range terminal[:3] { // oldest three
		if _, err := Load(id); !phelixerr.IsCode(err, phelixerr.CodeNotFound) {
			t.Fatalf("old terminal %s should be pruned, got %v", id, err)
		}
	}
	for _, id := range terminal[3:] { // newest two
		if _, err := Load(id); err != nil {
			t.Fatalf("newest terminal %s must be kept: %v", id, err)
		}
	}
}

// TestResolveDeploymentsUseSingleOpsScan proves Resolve correlates K deployment
// references with exactly ONE operation scan, not K. (Task 5.)
func TestResolveDeploymentsUseSingleOpsScan(t *testing.T) {
	isolate(t)
	s := mustCreate(t, CreateOpts{})
	var deps []string
	for i := 0; i < 6; i++ {
		deps = append(deps, fmt.Sprintf("dep-%016x", i))
	}
	if _, err := Checkpoint(s.SessionID, CheckpointOpts{Refs: Refs{Deployments: deps}, ExpectRev: -1}); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	loaded, err := Load(s.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	var scans int
	orig := opsListForResolve
	opsListForResolve = func(app string, limit int) ([]*ops.Record, int, error) {
		scans++
		return orig(app, limit)
	}
	t.Cleanup(func() { opsListForResolve = orig })

	res := Resolve(loaded)
	if scans != 1 {
		t.Fatalf("Resolve performed %d operation scans for %d deployment refs, want exactly 1", scans, len(deps))
	}
	if len(res.Deployments) != len(deps) {
		t.Fatalf("resolved %d deployments, want %d", len(res.Deployments), len(deps))
	}
}

// TestPruneTerminalRemovesLockArtifacts proves Task 8: pruning a terminal
// session removes its lock file and its in-process lock-map entry, so neither
// accumulates without bound over a long-lived host's lifetime.
func TestPruneTerminalRemovesLockArtifacts(t *testing.T) {
	isolate(t)
	older := mustCreate(t, CreateOpts{})
	if _, err := Checkpoint(older.SessionID, CheckpointOpts{Note: "x", ExpectRev: -1}); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if _, err := Complete(older.SessionID, CompleteOpts{ExpectRev: -1}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	newer := mustCreate(t, CreateOpts{})
	if _, err := Complete(newer.SessionID, CompleteOpts{ExpectRev: -1}); err != nil {
		t.Fatalf("Complete newer: %v", err)
	}

	olderLock, _ := lockFilePath(older.SessionID)
	if _, err := os.Stat(olderLock); err != nil {
		t.Fatalf("older lock file should exist before prune: %v", err)
	}
	locksMu.Lock()
	_, had := idLocks[older.SessionID]
	locksMu.Unlock()
	if !had {
		t.Fatal("older session should have an in-proc lock entry after mutation")
	}

	if _, err := PruneTerminal(1); err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}

	if _, err := Load(older.SessionID); !phelixerr.IsCode(err, phelixerr.CodeNotFound) {
		t.Fatalf("older terminal session should be pruned, got %v", err)
	}
	if _, err := os.Stat(olderLock); !os.IsNotExist(err) {
		t.Fatalf("older lock file should be removed, stat err=%v", err)
	}
	locksMu.Lock()
	_, still := idLocks[older.SessionID]
	locksMu.Unlock()
	if still {
		t.Fatal("older session in-proc lock entry should be dropped")
	}
	if _, err := Load(newer.SessionID); err != nil {
		t.Fatalf("newer session must survive: %v", err)
	}
}
