package job

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testExecutor(t *testing.T) (*Executor, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	return NewExecutor(NewStore(t.TempDir())), dir
}

func TestExecutorSuccessWritesRecordAndLog(t *testing.T) {
	executor, dir := testExecutor(t)
	record, err := executor.Run(context.Background(), RunRequest{
		AppName: "api",
		Dir:     dir,
		Command: []string{"/bin/sh", "-c", "echo hello; echo warn >&2"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if record.Status != StatusSucceeded || record.Attempts != 1 || record.ExitCode != 0 {
		t.Fatalf("record = %+v", record)
	}
	if record.FinishedAt == nil || record.DurationMS < 0 {
		t.Errorf("finish metadata = %+v", record)
	}
	logPath, err := executor.Store.LogPath("api", record.ID)
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(raw), "hello") || !strings.Contains(string(raw), "warn") {
		t.Errorf("log = %q", raw)
	}
	stored, err := executor.Store.Load("api", record.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if stored.Status != StatusSucceeded {
		t.Errorf("stored status = %q", stored.Status)
	}
}

func TestExecutorRetriesSequentially(t *testing.T) {
	executor, dir := testExecutor(t)
	counter := filepath.Join(dir, "count")
	command := fmt.Sprintf("echo x >> %q; exit 3", counter)
	record, err := executor.Run(context.Background(), RunRequest{
		AppName: "api",
		Dir:     dir,
		Command: []string{"/bin/sh", "-c", command},
		Retries: 2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if record.Status != StatusFailed || record.Attempts != 3 || record.ExitCode != 3 {
		t.Fatalf("record = %+v", record)
	}
	raw, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if lines := strings.Count(string(raw), "x"); lines != 3 {
		t.Errorf("attempts recorded on disk = %d, want 3", lines)
	}
}

func TestExecutorTimeoutKillsProcessGroup(t *testing.T) {
	executor, dir := testExecutor(t)
	start := time.Now()
	record, err := executor.Run(context.Background(), RunRequest{
		AppName: "api",
		Dir:     dir,
		Command: []string{"/bin/sh", "-c", "sleep 5 & wait"},
		Timeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if record.Status != StatusTimedOut {
		t.Fatalf("status = %q, want timed_out", record.Status)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("timeout took %s", elapsed)
	}
}

func TestExecutorCancellation(t *testing.T) {
	executor, dir := testExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	record, err := executor.Run(ctx, RunRequest{
		AppName: "api",
		Dir:     dir,
		Command: []string{"/bin/sh", "-c", "sleep 5"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if record.Status != StatusCancelled {
		t.Fatalf("status = %q, want cancelled", record.Status)
	}
}

func TestExecutorConcurrencyPolicy(t *testing.T) {
	executor, dir := testExecutor(t)
	lock, err := executor.Store.TryAcquireRunLock("api")
	if err != nil {
		t.Fatalf("TryAcquireRunLock: %v", err)
	}
	req := RunRequest{
		AppName: "api",
		Dir:     dir,
		Command: []string{"/bin/sh", "-c", "true"},
	}
	if _, err := executor.Run(context.Background(), req); !errors.Is(err, ErrRunLockHeld) {
		t.Fatalf("Run under a held lock = %v, want ErrRunLockHeld", err)
	}
	req.Concurrency = ConcurrencyAllow
	record, err := executor.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run with allow: %v", err)
	}
	if record.Status != StatusSucceeded {
		t.Fatalf("status = %q", record.Status)
	}
	lock.Release()
}

func TestExecutorSkipRecordsOccurrence(t *testing.T) {
	executor, dir := testExecutor(t)
	when := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	record, err := executor.Skip(RunRequest{AppName: "api", Dir: dir, Command: []string{"true"}}, when, "busy")
	if err != nil {
		t.Fatalf("Skip: %v", err)
	}
	if record.Status != StatusSkipped || record.Error != "busy" || record.ScheduledFor == nil || !record.ScheduledFor.Equal(when) {
		t.Fatalf("record = %+v", record)
	}
	if !executor.Store.HasScheduled("api", when) {
		t.Error("skipped occurrence was not persisted")
	}
}

func TestResolveCommand(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "backup")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	exe, args, err := resolveCommand(dir, []string{"./backup", "--full"}, "")
	if err != nil || exe != script || len(args) != 1 || args[0] != "--full" {
		t.Fatalf("relative = %q %q %v", exe, args, err)
	}
	exe, _, err = resolveCommand(dir, []string{"./missing"}, "/versions/v1/binary")
	if err != nil || exe != "/versions/v1/binary" {
		t.Fatalf("fallback = %q %v", exe, err)
	}
	if _, _, err := resolveCommand(dir, []string{"./missing"}, ""); err == nil {
		t.Fatal("missing command without a version must fail")
	}
	exe, _, err = resolveCommand(dir, []string{"sh"}, "")
	if err != nil || exe != "sh" {
		t.Fatalf("bare = %q %v", exe, err)
	}
	if _, _, err := resolveCommand(dir, []string{"  "}, ""); err == nil {
		t.Fatal("blank executable must fail")
	}
}
