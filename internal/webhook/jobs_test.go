package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func newTestJobStore(t *testing.T) *JobStore {
	t.Helper()
	s := NewJobStore(filepath.Join(t.TempDir(), "jobs"), DefaultJobRetention)
	if err := s.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	return s
}

func newJobRecord(app, delivery, commit string) *JobRecord {
	return &JobRecord{
		DeliveryID: delivery,
		AppID:      "app-1",
		AppName:    app,
		Branch:     "main",
		Commit:     commit,
		Provider:   ProviderGitHub,
		Status:     StatusAccepted,
		Stage:      StageQueue,
		AcceptedAt: time.Now().UnixMilli(),
	}
}

func mustTransition(t *testing.T, what string, fn func() error) {
	t.Helper()
	if err := fn(); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func wantStatusIs(t *testing.T, s *JobStore, id, status string) {
	t.Helper()
	rec, ok := s.Get(id)
	if !ok {
		t.Fatalf("job %s missing", id)
	}
	if rec.Status != status {
		t.Fatalf("job %s status = %s, want %s", id, rec.Status, status)
	}
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

func TestJobStore_Persistence(t *testing.T) {
	t.Run("create, update, reload", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "jobs")
		s := NewJobStore(dir, DefaultJobRetention)
		if err := s.Load(); err != nil {
			t.Fatal(err)
		}
		rec := newJobRecord("api", "d-persist", "abcdef1234567890abcdef1234567890abcdef12")
		if err := s.Create(rec); err != nil {
			t.Fatal(err)
		}
		mustTransition(t, "queued", func() error { return s.MarkQueued(rec.ID) })
		mustTransition(t, "syncing", func() error { return s.MarkSyncing(rec.ID) })
		mustTransition(t, "building", func() error { return s.MarkBuilding(rec.ID) })
		mustTransition(t, "succeeded", func() error { return s.MarkSucceeded(rec.ID, 7) })

		// A fresh store over the same directory sees the full lifecycle.
		s2 := NewJobStore(dir, DefaultJobRetention)
		if err := s2.Load(); err != nil {
			t.Fatal(err)
		}
		got, ok := s2.Get(rec.ID)
		if !ok {
			t.Fatal("job lost after reload")
		}
		if got.Status != StatusSucceeded || got.Version != 7 || got.Stage != StageCleanup {
			t.Fatalf("reloaded record = %+v", got)
		}
		if got.FinishedAt == 0 || got.StartedAt == 0 {
			t.Fatalf("timestamps missing: %+v", got)
		}
		if got.Commit != "abcdef1234567890abcdef1234567890abcdef12" {
			t.Fatalf("commit replaced: %s", got.Commit)
		}
	})

	t.Run("job ids are wh_ prefixed and unique", func(t *testing.T) {
		seen := make(map[string]bool)
		for i := 0; i < 100; i++ {
			id := NewJobID()
			if !jobIDPattern.MatchString(id) {
				t.Fatalf("id %q does not match the expected shape", id)
			}
			if seen[id] {
				t.Fatalf("duplicate id %s", id)
			}
			seen[id] = true
		}
	})

	t.Run("malformed record is quarantined, history survives", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "jobs")
		s := NewJobStore(dir, DefaultJobRetention)
		if err := s.Load(); err != nil {
			t.Fatal(err)
		}
		good := newJobRecord("api", "d-good", "abcdef1234567890abcdef1234567890abcdef12")
		if err := s.Create(good); err != nil {
			t.Fatal(err)
		}
		// Simulate a partially-written record from a crash.
		if err := os.WriteFile(filepath.Join(dir, "wh_deadbeef00000000.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}

		s2 := NewJobStore(dir, DefaultJobRetention)
		if err := s2.Load(); err != nil {
			t.Fatalf("load with corrupt record: %v", err)
		}
		if _, ok := s2.Get(good.ID); !ok {
			t.Fatal("good record lost because of a corrupt sibling")
		}
		if _, ok := s2.Get("wh_deadbeef00000000"); ok {
			t.Fatal("corrupt record was loaded")
		}
		// The corrupt file was moved aside, not deleted.
		matches, _ := filepath.Glob(filepath.Join(dir, "*.corrupt.*"))
		if len(matches) == 0 {
			t.Fatal("corrupt record was not quarantined")
		}
	})

	t.Run("read-only store never repairs or mutates", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "jobs")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "wh_deadbeef00000000.json"), []byte("{bad"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := OpenJobStoreReadOnly(dir)
		if err := s.Load(); err != nil {
			t.Fatal(err)
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "*.corrupt.*"))
		if len(matches) != 0 {
			t.Fatal("read-only store renamed a file")
		}
		if err := s.Create(newJobRecord("api", "d-x", "abcdef1234567890abcdef1234567890abcdef12")); err == nil {
			t.Fatal("read-only store accepted a write")
		}
	})

	t.Run("create failure is reported (durability gate)", func(t *testing.T) {
		// A store whose directory is a regular file cannot persist.
		blocked := filepath.Join(t.TempDir(), "notadir")
		if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := NewJobStore(blocked, DefaultJobRetention)
		if err := s.Load(); err == nil {
			// Load may tolerate it; Create must not.
			_ = err
		}
		if err := s.Create(newJobRecord("api", "d-blocked", "abcdef1234567890abcdef1234567890abcdef12")); err == nil {
			t.Fatal("Create must fail when the record cannot be persisted")
		}
	})

	t.Run("concurrent updates to different jobs are safe", func(t *testing.T) {
		s := newTestJobStore(t)
		const n = 16
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			rec := newJobRecord("api", fmt.Sprintf("d-conc-%d", i), "abcdef1234567890abcdef1234567890abcdef12")
			if err := s.Create(rec); err != nil {
				t.Fatal(err)
			}
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				_ = s.MarkQueued(id)
				_ = s.MarkSyncing(id)
				_ = s.MarkBuilding(id)
				_ = s.MarkProgress(id, StatusDeploying, StageDeploy)
				_ = s.MarkProgress(id, StatusHealthChecking, StageHealthCheck)
				_ = s.MarkSucceeded(id, 1)
			}(rec.ID)
		}
		// Concurrent readers while the writers run.
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					_ = s.ActiveForApp("api")
					_ = s.HistoryForApp("api", 10)
				}
			}()
		}
		wg.Wait()
		if got := s.Count(); got != n {
			t.Fatalf("store has %d jobs, want %d", got, n)
		}
		for i := 0; i < n; i++ {
			rec, ok := s.Get(fmt.Sprintf("%s", mustID(t, s, i)))
			_ = rec
			if !ok {
				t.Fatal("job lost")
			}
		}
	})
}

func mustID(t *testing.T, s *JobStore, i int) string {
	t.Helper()
	// The concurrent test uses generated ids; recover them via history.
	recs := s.HistoryForApp("api", 1000)
	if len(recs) != 16 {
		t.Fatalf("history has %d records, want 16", len(recs))
	}
	return recs[i].ID
}

// ---------------------------------------------------------------------------
// Lifecycle state machine
// ---------------------------------------------------------------------------

func TestJobStore_Lifecycle(t *testing.T) {
	t.Run("happy path through every running state", func(t *testing.T) {
		s := newTestJobStore(t)
		rec := newJobRecord("api", "d-life", "abcdef1234567890abcdef1234567890abcdef12")
		mustTransition(t, "create", func() error { return s.Create(rec) })
		mustTransition(t, "queued", func() error { return s.MarkQueued(rec.ID) })
		mustTransition(t, "syncing", func() error { return s.MarkSyncing(rec.ID) })
		mustTransition(t, "building", func() error { return s.MarkBuilding(rec.ID) })
		mustTransition(t, "deploying", func() error { return s.MarkProgress(rec.ID, StatusDeploying, StageDeploy) })
		mustTransition(t, "health", func() error { return s.MarkProgress(rec.ID, StatusHealthChecking, StageHealthCheck) })
		mustTransition(t, "cleanup", func() error { return s.MarkCleanup(rec.ID) })
		mustTransition(t, "succeeded", func() error { return s.MarkSucceeded(rec.ID, 3) })

		got, _ := s.Get(rec.ID)
		if got.Status != StatusSucceeded || got.Version != 3 {
			t.Fatalf("record = %+v", got)
		}
	})

	t.Run("git sync failure terminal from syncing", func(t *testing.T) {
		s := newTestJobStore(t)
		rec := newJobRecord("api", "d-gitfail", "abcdef1234567890abcdef1234567890abcdef12")
		mustTransition(t, "create", func() error { return s.Create(rec) })
		mustTransition(t, "syncing", func() error { return s.MarkSyncing(rec.ID) })
		mustTransition(t, "failed", func() error { return s.MarkFailed(rec.ID, JobErrGitSync, "fetch failed") })
		got, _ := s.Get(rec.ID)
		if got.ErrorCode != JobErrGitSync || got.ErrorMessage != "fetch failed" {
			t.Fatalf("record = %+v", got)
		}
	})

	t.Run("rolled back terminal", func(t *testing.T) {
		s := newTestJobStore(t)
		rec := newJobRecord("api", "d-rb", "abcdef1234567890abcdef1234567890abcdef12")
		mustTransition(t, "create", func() error { return s.Create(rec) })
		mustTransition(t, "queued", func() error { return s.MarkQueued(rec.ID) })
		mustTransition(t, "syncing", func() error { return s.MarkSyncing(rec.ID) })
		mustTransition(t, "building", func() error { return s.MarkBuilding(rec.ID) })
		mustTransition(t, "deploying", func() error { return s.MarkProgress(rec.ID, StatusDeploying, StageDeploy) })
		mustTransition(t, "rolled back", func() error {
			return s.MarkRolledBack(rec.ID, 12, JobErrDeploy, "health check failed")
		})
		got, _ := s.Get(rec.ID)
		if got.Status != StatusRolledBack || got.Version != 12 {
			t.Fatalf("record = %+v", got)
		}
	})

	t.Run("terminal states are final", func(t *testing.T) {
		s := newTestJobStore(t)
		rec := newJobRecord("api", "d-final", "abcdef1234567890abcdef1234567890abcdef12")
		mustTransition(t, "create", func() error { return s.Create(rec) })
		mustTransition(t, "queued", func() error { return s.MarkQueued(rec.ID) })
		mustTransition(t, "failed", func() error { return s.MarkFailed(rec.ID, JobErrBuild, "boom") })
		for _, fn := range []func() error{
			func() error { return s.MarkSucceeded(rec.ID, 1) },
			func() error { return s.MarkFailed(rec.ID, JobErrBuild, "again") },
			func() error { return s.MarkSyncing(rec.ID) },
			func() error { return s.MarkRolledBack(rec.ID, 1, JobErrDeploy, "x") },
		} {
			if err := fn(); err == nil {
				t.Fatal("terminal job accepted another transition")
			}
		}
	})

	t.Run("running states never move backward", func(t *testing.T) {
		s := newTestJobStore(t)
		rec := newJobRecord("api", "d-back", "abcdef1234567890abcdef1234567890abcdef12")
		mustTransition(t, "create", func() error { return s.Create(rec) })
		mustTransition(t, "syncing", func() error { return s.MarkSyncing(rec.ID) })
		mustTransition(t, "building", func() error { return s.MarkBuilding(rec.ID) })
		if err := s.MarkSyncing(rec.ID); err == nil {
			t.Fatal("building → syncing must be rejected")
		}
		if err := s.MarkProgress(rec.ID, StatusBuilding, StageBuild); err != nil {
			t.Fatalf("same-rank stage refresh should be allowed: %v", err)
		}
	})

	t.Run("unknown job rejected", func(t *testing.T) {
		s := newTestJobStore(t)
		if err := s.MarkQueued("wh_0000000000000000"); err == nil {
			t.Fatal("unknown job accepted")
		}
	})
}

// ---------------------------------------------------------------------------
// Retention
// ---------------------------------------------------------------------------

func TestJobStore_Retention(t *testing.T) {
	t.Run("bounded history, active jobs never deleted", func(t *testing.T) {
		s := NewJobStore(filepath.Join(t.TempDir(), "jobs"), 5)
		if err := s.Load(); err != nil {
			t.Fatal(err)
		}
		// Six finished jobs: only the five newest survive.
		for i := 0; i < 6; i++ {
			rec := newJobRecord("api", fmt.Sprintf("d-ret-%d", i), "abcdef1234567890abcdef1234567890abcdef12")
			rec.AcceptedAt = time.Now().Add(-time.Duration(10-i) * time.Minute).UnixMilli()
			if err := s.Create(rec); err != nil {
				t.Fatal(err)
			}
			if err := s.MarkFailed(rec.ID, JobErrBuild, "done"); err != nil {
				t.Fatal(err)
			}
		}
		if got := s.Count(); got != 5 {
			t.Fatalf("retention kept %d records, want 5", got)
		}

		// An active job is never swept, even when over capacity.
		active := newJobRecord("api", "d-ret-active", "abcdef1234567890abcdef1234567890abcdef12")
		if err := s.Create(active); err != nil {
			t.Fatal(err)
		}
		mustTransition(t, "syncing", func() error { return s.MarkSyncing(active.ID) })
		for i := 0; i < 3; i++ {
			rec := newJobRecord("api", fmt.Sprintf("d-ret2-%d", i), "abcdef1234567890abcdef1234567890abcdef12")
			if err := s.Create(rec); err != nil {
				t.Fatal(err)
			}
			if err := s.MarkSucceeded(rec.ID, 1); err != nil {
				t.Fatal(err)
			}
		}
		if _, ok := s.Get(active.ID); !ok {
			t.Fatal("active job was deleted by retention")
		}
		if got := s.Count(); got > 5+1+3 {
			t.Fatalf("retention not applied: %d records", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Restart recovery
// ---------------------------------------------------------------------------

func TestJobStore_Recovery(t *testing.T) {
	commit := "abcdef1234567890abcdef1234567890abcdef12"

	t.Run("interrupted queued job fails explicitly", func(t *testing.T) {
		s := newTestJobStore(t)
		rec := newJobRecord("api", "d-rec-a", commit)
		mustTransition(t, "create", func() error { return s.Create(rec) })
		mustTransition(t, "queued", func() error { return s.MarkQueued(rec.ID) })

		outcomes := s.RecoverInterrupted(nil)
		if len(outcomes) != 1 || outcomes[0].Result != StatusFailed {
			t.Fatalf("outcomes = %+v", outcomes)
		}
		got, _ := s.Get(rec.ID)
		if got.Status != StatusFailed || got.ErrorCode != JobErrInterrupted || got.Stage != StageRecovery {
			t.Fatalf("record = %+v", got)
		}
		if !strings.Contains(got.ErrorMessage, "not re-run") {
			t.Fatalf("message should say the deployment was not re-run: %q", got.ErrorMessage)
		}
	})

	t.Run("interrupted syncing job fails explicitly", func(t *testing.T) {
		s := newTestJobStore(t)
		rec := newJobRecord("api", "d-rec-b", commit)
		mustTransition(t, "create", func() error { return s.Create(rec) })
		mustTransition(t, "syncing", func() error { return s.MarkSyncing(rec.ID) })
		s.RecoverInterrupted(nil)
		got, _ := s.Get(rec.ID)
		if got.Status != StatusFailed || got.ErrorCode != JobErrInterrupted {
			t.Fatalf("record = %+v", got)
		}
	})

	t.Run("deployment completed before crash is recovered as succeeded", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir()) // version state lives under $HOME/.phelix
		s := newTestJobStore(t)
		rec := newJobRecord("api", "d-rec-c", commit)
		mustTransition(t, "create", func() error { return s.Create(rec) })
		mustTransition(t, "syncing", func() error { return s.MarkSyncing(rec.ID) })
		mustTransition(t, "building", func() error { return s.MarkBuilding(rec.ID) })

		// The orphaned rebuild demonstrably finished: the app's current
		// version is the job's exact commit, promoted after acceptance.
		version := promoteTestVersion(t, "api", commit)

		outcomes := s.RecoverInterrupted(deploy.CurrentVersionMeta)
		if len(outcomes) != 1 || outcomes[0].Result != StatusSucceeded || outcomes[0].Version != version {
			t.Fatalf("outcomes = %+v (want succeeded with v%d)", outcomes, version)
		}
		got, _ := s.Get(rec.ID)
		if got.Status != StatusSucceeded || got.Version != version {
			t.Fatalf("record = %+v", got)
		}
	})

	t.Run("deployment not completed is recovered as interrupted", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		s := newTestJobStore(t)
		rec := newJobRecord("api", "d-rec-d", commit)
		mustTransition(t, "create", func() error { return s.Create(rec) })
		mustTransition(t, "syncing", func() error { return s.MarkSyncing(rec.ID) })
		mustTransition(t, "building", func() error { return s.MarkBuilding(rec.ID) })
		mustTransition(t, "health", func() error { return s.MarkProgress(rec.ID, StatusHealthChecking, StageHealthCheck) })

		// The current version is an OLDER commit: the orphan never promoted.
		promoteTestVersion(t, "api", strings.Repeat("0", 40))

		outcomes := s.RecoverInterrupted(deploy.CurrentVersionMeta)
		if len(outcomes) != 1 || outcomes[0].Result != StatusFailed {
			t.Fatalf("outcomes = %+v", outcomes)
		}
		got, _ := s.Get(rec.ID)
		if got.Status != StatusFailed || got.ErrorCode != JobErrInterrupted {
			t.Fatalf("record = %+v", got)
		}
	})

	t.Run("recovery never re-runs anything and is idempotent", func(t *testing.T) {
		s := newTestJobStore(t)
		rec := newJobRecord("api", "d-rec-e", commit)
		mustTransition(t, "create", func() error { return s.Create(rec) })
		mustTransition(t, "queued", func() error { return s.MarkQueued(rec.ID) })
		s.RecoverInterrupted(nil)
		// A second pass has nothing left to resolve.
		if outcomes := s.RecoverInterrupted(nil); len(outcomes) != 0 {
			t.Fatalf("second recovery produced %+v", outcomes)
		}
	})
}

// promoteTestVersion records and promotes a version through the real deploy
// package APIs so recovery/correlation logic is tested against real state.
// Returns the promoted version number.
func promoteTestVersion(t *testing.T, app string, commit string) int {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.WriteFile(bin, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec, err := deploy.RecordFreshBuild(app, "app-1", bin, commit, "", nil, deploy.DefaultRetention{Max: 5}, nil)
	if err != nil {
		t.Fatalf("RecordFreshBuild: %v", err)
	}
	if err := deploy.PromoteVersion(app, rec.Version, "classic"); err != nil {
		t.Fatalf("PromoteVersion: %v", err)
	}
	return rec.Version
}

// ---------------------------------------------------------------------------
// Failure classification (structured signals, no log guessing)
// ---------------------------------------------------------------------------

func TestClassifyRebuildFailure(t *testing.T) {
	tail := func(lines ...string) []string { return lines }
	exitErr := func(code int) error {
		// A real *exec.ExitError from a subprocess exiting with that code.
		err := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", code)).Run()
		if err == nil {
			t.Fatalf("subprocess unexpectedly succeeded for exit code %d", code)
		}
		return err
	}

	cases := []struct {
		name     string
		tail     []string
		err      error
		wantCode string
	}{
		{"rendered build code", tail("Error: rebuild failed", "  Code: BUILD_FAILED"), nil, JobErrBuild},
		{"rendered health code", tail("  Code: HEALTH_CHECK_FAILED"), nil, JobErrHealth},
		{"rendered deploy code", tail("  Code: DEPLOY_FAILED"), nil, JobErrDeploy},
		{"rendered auto-rollback", tail("  Code: AUTO_ROLLBACK_FAILED"), nil, JobErrAutoRoll},
		{"last rendered code wins", tail("  Code: DEPLOY_FAILED", "  Code: HEALTH_CHECK_FAILED"), nil, JobErrHealth},
		{"exit code build", nil, exitErr(rebuildExitBuild), JobErrBuild},
		{"exit code deploy", nil, exitErr(rebuildExitDeploy), JobErrDeploy},
		{"exit code auto rollback", nil, exitErr(rebuildExitAutoRoll), JobErrAutoRoll},
		{"unknown exit code", nil, exitErr(1), JobErrRebuild},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRebuildFailure(tc.tail, tc.err); got != tc.wantCode {
				t.Fatalf("classify = %s, want %s", got, tc.wantCode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Handler durability gate
// ---------------------------------------------------------------------------

func TestWebhookHandler_DurableJobCreation(t *testing.T) {
	t.Run("accepted delivery creates a durable queued job", func(t *testing.T) {
		h := newHarness(t)
		status, resp := h.postPush(t, "api", "d-job-1", "refs/heads/main")
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, true, "webhook accepted")

		active := h.jobs.ActiveForApp("api")
		if len(active) != 1 {
			t.Fatalf("active jobs = %d, want 1", len(active))
		}
		rec := active[0]
		if rec.DeliveryID != "d-job-1" || rec.Commit != testCommit || rec.Branch != "main" {
			t.Fatalf("record = %+v", rec)
		}
		if rec.Status != StatusQueued {
			t.Fatalf("status = %s, want queued", rec.Status)
		}
		if !strings.HasPrefix(rec.ID, "wh_") {
			t.Fatalf("job id %q is not wh_-prefixed", rec.ID)
		}
	})

	t.Run("persistence failure refuses acceptance and rolls the ledger back", func(t *testing.T) {
		h := newHarness(t)
		// Break persistence after the harness is built: a job store whose
		// directory is a regular file cannot write records.
		blocked := filepath.Join(t.TempDir(), "notadir")
		if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		broken := NewJobStore(blocked, DefaultJobRetention)
		_ = broken.Load()
		// Swap into a fresh server over the same ledger/queue.
		brokenSrv := NewServer(ServerConfig{}, Dependencies{
			Resolver:     h.resolver,
			Ledger:       h.ledger,
			Queue:        h.queue,
			Jobs:         broken,
			LookupSecret: func(name string) (string, bool) { v, ok := h.secrets[name]; return v, ok },
			ReportEvent:  h.events.record,
		})
		brokenTS := httptest.NewServer(brokenSrv.Handler())
		defer brokenTS.Close()

		body := pushBody(t, "refs/heads/main", testCommit)
		req, _ := http.NewRequest(http.MethodPost, brokenTS.URL+"/webhook/api", bytes.NewReader(body))
		req.Header.Set(SignatureHeader, sign(testSecret, body))
		req.Header.Set(DeliveryHeader, "d-job-blocked")
		req.Header.Set(EventTypeHeader, "push")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (never acknowledge without a durable record)", resp.StatusCode)
		}
		if n := h.rebuild.jobCount(); n != 0 {
			t.Fatalf("%d jobs enqueued without a durable record", n)
		}
		// The delivery was rolled back: the SAME delivery is accepted once
		// the store works again.
		healthy := NewServer(ServerConfig{}, Dependencies{
			Resolver:     h.resolver,
			Ledger:       h.ledger,
			Queue:        h.queue,
			Jobs:         h.jobs,
			LookupSecret: func(name string) (string, bool) { v, ok := h.secrets[name]; return v, ok },
			ReportEvent:  h.events.record,
		})
		ts2 := httptest.NewServer(healthy.Handler())
		defer ts2.Close()
		req2, _ := http.NewRequest(http.MethodPost, ts2.URL+"/webhook/api", bytes.NewReader(body))
		req2.Header.Set(SignatureHeader, sign(testSecret, body))
		req2.Header.Set(DeliveryHeader, "d-job-blocked")
		req2.Header.Set(EventTypeHeader, "push")
		resp2, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("redelivery status = %d, want 200 after the ledger rollback", resp2.StatusCode)
		}
		if n := h.jobs.ActiveForApp("api"); len(n) != 1 {
			t.Fatalf("active jobs after redelivery = %d, want 1", len(n))
		}
	})

	t.Run("duplicate delivery while the original job is running", func(t *testing.T) {
		h := newHarness(t)
		h.rebuild.gate = make(chan struct{})
		defer close(h.rebuild.gate)

		status, resp := h.postPush(t, "api", "d-job-run", "refs/heads/main")
		wantStatus(t, status, http.StatusOK)
		h.rebuild.waitJobs(t, 1)

		// The same delivery arrives again while the job is executing.
		status, resp = h.postPush(t, "api", "d-job-run", "refs/heads/main")
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, false, "delivery already processed")

		if n := h.rebuild.jobCount(); n != 1 {
			t.Fatalf("duplicate delivery while running created %d jobs", n)
		}
		if n := len(h.jobs.ActiveForApp("api")); n != 1 {
			t.Fatalf("active records = %d, want 1", n)
		}
	})
}

// ---------------------------------------------------------------------------
// Full lifecycle through CliRebuild with real deploy state
// ---------------------------------------------------------------------------

func TestJobLifecycle_ThroughCliRebuild(t *testing.T) {
	requireGit(t)
	t.Setenv("HOME", t.TempDir())

	repo := newGitRepo(t)
	jobs := newTestJobStore(t)
	prep, _ := newTestPreparer(t)

	t.Run("success correlates the deployed version by exact commit", func(t *testing.T) {
		// The rebuild "succeeds" and the pipeline promotes a version built
		// from exactly the pushed commit.
		rb := &CliRebuild{
			poll:    5 * time.Millisecond,
			prepare: prep,
			jobs:    jobs,
			run: func(ctx context.Context, job *Job, dir string, args ...string) ([]string, error) {
				promoteTestVersion(t, "api", job.CommitSHA)
				return nil, nil
			},
		}
		job := jobFor(repo, repo.commitB, "d-life-ok")
		job.AppName = "api"
		rec := newJobRecord("api", job.DeliveryID, job.CommitSHA)
		rec.ID = ""
		if err := jobs.Create(rec); err != nil {
			t.Fatal(err)
		}
		job.ID = rec.ID
		mustTransition(t, "queued", func() error { return jobs.MarkQueued(rec.ID) })

		if err := rb.Rebuild(context.Background(), job); err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		got, _ := jobs.Get(rec.ID)
		if got.Status != StatusSucceeded {
			t.Fatalf("status = %s (%s: %s)", got.Status, got.ErrorCode, got.ErrorMessage)
		}
		if got.Version == 0 {
			t.Fatal("deployed version was not correlated")
		}
		// The exact pushed commit is what the version is tied to.
		meta, err := deploy.CurrentVersionMeta("api")
		if err != nil || meta == nil || meta.GitCommit != repo.commitB || meta.Version != got.Version {
			t.Fatalf("correlated version does not match the pushed commit: %+v %v", meta, err)
		}
	})

	t.Run("failed deploy with automatic rollback is recorded as rolled_back", func(t *testing.T) {
		// Baseline rollback history is empty; the "rebuild" fails with the
		// CLI's rendered deploy-failure code AND records an automatic
		// rollback entry — exactly what the real pipeline does.
		rb := &CliRebuild{
			poll:    5 * time.Millisecond,
			prepare: prep,
			jobs:    jobs,
			run: func(ctx context.Context, job *Job, dir string, args ...string) ([]string, error) {
				deploy.RecordRollbackResultSource("api2", 5, 4, "classic", "health", nil, nil, deploy.RollbackSourceAutomatic)
				return []string{"Error: deployment of v5 failed", "  Code: HEALTH_CHECK_FAILED"},
					phelixerr.Newf(phelixerr.CodeProcessFailed, "webhook: rebuild command failed")
			},
		}
		job := jobFor(repo, repo.commitA, "d-life-rb")
		job.AppName = "api2"
		rec := newJobRecord("api2", job.DeliveryID, job.CommitSHA)
		if err := jobs.Create(rec); err != nil {
			t.Fatal(err)
		}
		job.ID = rec.ID

		if err := rb.Rebuild(context.Background(), job); err == nil {
			t.Fatal("expected the rebuild failure to surface")
		}
		got, _ := jobs.Get(rec.ID)
		if got.Status != StatusRolledBack {
			t.Fatalf("status = %s (%s: %s), want rolled_back", got.Status, got.ErrorCode, got.ErrorMessage)
		}
		if got.Version != 4 {
			t.Fatalf("restored version = %d, want 4 (from the rollback record)", got.Version)
		}
		if got.ErrorCode != JobErrHealth {
			t.Fatalf("error code = %s, want HEALTH_CHECK_FAILED", got.ErrorCode)
		}
	})

	t.Run("git sync failure marks the job failed before any rebuild", func(t *testing.T) {
		rb := &CliRebuild{
			poll:    5 * time.Millisecond,
			prepare: prep,
			jobs:    jobs,
			run: func(ctx context.Context, job *Job, dir string, args ...string) ([]string, error) {
				t.Fatal("rebuild must not run after a git sync failure")
				return nil, nil
			},
		}
		job := jobFor(repo, "ffffffffffffffffffffffffffffffffffffffff", "d-life-git")
		job.AppName = "api3"
		rec := newJobRecord("api3", job.DeliveryID, job.CommitSHA)
		if err := jobs.Create(rec); err != nil {
			t.Fatal(err)
		}
		job.ID = rec.ID

		if err := rb.Rebuild(context.Background(), job); err == nil {
			t.Fatal("expected a git sync failure")
		}
		got, _ := jobs.Get(rec.ID)
		if got.Status != StatusFailed || got.ErrorCode != JobErrGitSync {
			t.Fatalf("record = %+v", got)
		}
		if got.StartedAt == 0 || got.FinishedAt == 0 {
			t.Fatalf("timestamps missing: %+v", got)
		}
	})

	t.Run("stage detector advances deploy and health from pipeline output", func(t *testing.T) {
		jobs := newTestJobStore(t)
		job := &Job{ID: NewJobID(), AppName: "api", DeliveryID: "d-stage", CommitSHA: testCommit}
		rec := &JobRecord{ID: job.ID, DeliveryID: job.DeliveryID, AppName: "api", Commit: testCommit, Status: StatusAccepted, AcceptedAt: time.Now().UnixMilli()}
		if err := jobs.Create(rec); err != nil {
			t.Fatal(err)
		}
		mustTransition(t, "syncing", func() error { return jobs.MarkSyncing(job.ID) })
		mustTransition(t, "building", func() error { return jobs.MarkBuilding(job.ID) })

		det := newStageDetector(job, jobs)
		for _, line := range []string{
			"  → Rebuilding with go1.27...",
			"  → starting new instance on slot green",
			"    health tier selected: tier1-http",
			"  → Starting application on port 3000...", // late classic marker must not regress
		} {
			if _, err := det.Write([]byte(line + "\n")); err != nil {
				t.Fatal(err)
			}
		}
		got, _ := jobs.Get(job.ID)
		if got.Status != StatusHealthChecking || got.Stage != StageHealthCheck {
			t.Fatalf("record = %+v, want health_checking/health_check", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Message sanitization
// ---------------------------------------------------------------------------

func TestSanitizeJobMessage(t *testing.T) {
	long := strings.Repeat("x", 500)
	if got := SanitizeJobMessage(long); len(got) > 300+3 {
		t.Fatalf("message not bounded: %d", len(got))
	}
	if got := SanitizeJobMessage("  spaced  "); got != "spaced" {
		t.Fatalf("message not trimmed: %q", got)
	}
}

func TestJobEventMessage(t *testing.T) {
	rec := &JobRecord{ID: "wh_0011223344556677", DeliveryID: "d-1", Branch: "main",
		Commit: "abcdef123", Stage: StageBuild, Version: 9}
	msg := JobEventMessage(rec)
	for _, want := range []string{"job=wh_0011223344556677", "delivery=d-1", "branch=main", "commit=abcdef123", "stage=build", "version=v9"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q missing %q", msg, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func TestJobRecordJSONShape(t *testing.T) {
	rec := JobRecord{SchemaVersion: 1, ID: "wh_0011223344556677", Status: StatusQueued}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "delivery_id", "app_id", "app_name", "branch", "commit", "status", "stage", "accepted_at"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("json record missing key %q: %s", key, data)
		}
	}
	for _, banned := range []string{"secret", "worktree", "body"} {
		if strings.Contains(strings.ToLower(string(data)), banned) {
			t.Fatalf("json record leaks %q: %s", banned, data)
		}
	}
}
