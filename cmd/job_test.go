package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/job"
)

func withJobManager(t *testing.T, apps map[string]*app.AppInfo) {
	t.Helper()
	orig := app.Manager
	origStateDir := app.StateDirForTesting()
	if err := app.SetStateDirForTesting(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.SetStateDirForTesting(origStateDir); err != nil {
			t.Errorf("restore app state dir: %v", err)
		}
	})
	manager := &app.AppManager{Apps: map[string]*app.AppInfo{}}
	for id, info := range apps {
		manager.Apps[id] = info
	}
	app.Manager = manager
	if err := manager.SaveState(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Manager = orig })
}

func writeJobProject(t *testing.T, name string, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "phelix.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func seedJobApp(t *testing.T, name, dir string) {
	t.Helper()
	withJobManager(t, map[string]*app.AppInfo{
		"app-" + name: {ID: "app-" + name, Name: name, Directory: dir},
	})
}

func TestJobTargetResolvesJobConfig(t *testing.T) {
	dir := writeJobProject(t, "nightly", "name: nightly\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\njob:\n  command: [\"./backup\"]\n")
	seedJobApp(t, "nightly", dir)

	info, cfg, err := jobTarget("nightly")
	if err != nil {
		t.Fatalf("jobTarget: %v", err)
	}
	if info.Name != "nightly" || !cfg.IsJob() {
		t.Fatalf("unexpected target: %+v %v", info, cfg)
	}
}

func TestJobTargetRejectsNonJob(t *testing.T) {
	dir := writeJobProject(t, "web", "name: web\nport: 8080\n")
	seedJobApp(t, "web", dir)

	if _, _, err := jobTarget("web"); !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
		t.Fatalf("service app must be rejected, got %v", err)
	}
}

func TestJobTargetRejectsUnsafeName(t *testing.T) {
	dir := writeJobProject(t, "evil", "name: evil\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\njob:\n  command: [\"./backup\"]\n")
	withJobManager(t, map[string]*app.AppInfo{
		"app-evil": {ID: "app-evil", Name: "../evil", Directory: dir},
	})

	if _, _, err := jobTarget("../evil"); !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
		t.Fatalf("unsafe job name must be rejected, got %v", err)
	}
}

func TestJobRunHistoryAndLogs(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dataDir)
	t.Setenv("HOME", t.TempDir())
	dir := writeJobProject(t, "echoes", "name: echoes\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\njob:\n  command: [\"sh\", \"-c\", \"echo job-output\"]\n")
	seedJobApp(t, "echoes", dir)

	jobRunCmd.SetContext(context.Background())
	oldLimit, oldExecution := jobHistoryLimit, jobLogsExecution
	t.Cleanup(func() { jobHistoryLimit, jobLogsExecution = oldLimit, oldExecution })

	out := captureStdout(t, func() {
		if err := jobRunCmd.RunE(jobRunCmd, []string{"echoes"}); err != nil {
			t.Fatalf("job run: %v", err)
		}
	})
	if !strings.Contains(out, "succeeded") {
		t.Fatalf("run output must report success: %s", out)
	}

	records, err := job.NewStore(dataDir).List("echoes", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Status != job.StatusSucceeded {
		t.Fatalf("expected one succeeded record, got %+v", records)
	}

	history := captureStdout(t, func() {
		jobHistoryLimit = 5
		if err := jobHistoryCmd.RunE(jobHistoryCmd, []string{"echoes"}); err != nil {
			t.Fatalf("job history: %v", err)
		}
	})
	if !strings.Contains(history, records[0].ID) || !strings.Contains(history, "succeeded") {
		t.Fatalf("history must show the execution: %s", history)
	}

	jobLogsExecution = records[0].ID
	logsOut := captureStdout(t, func() {
		if err := jobLogsCmd.RunE(jobLogsCmd, []string{"echoes"}); err != nil {
			t.Fatalf("job logs: %v", err)
		}
	})
	if !strings.Contains(logsOut, "job-output") {
		t.Fatalf("logs must contain command output: %s", logsOut)
	}
}

func TestJobLogsRejectsPathTraversalExecutionID(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dataDir)
	dir := writeJobProject(t, "safe", "name: safe\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\njob:\n  command: [\"true\"]\n")
	seedJobApp(t, "safe", dir)

	oldExecution := jobLogsExecution
	t.Cleanup(func() { jobLogsExecution = oldExecution })
	jobLogsExecution = "../../../../etc/passwd"

	if err := jobLogsCmd.RunE(jobLogsCmd, []string{"safe"}); !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
		t.Fatalf("unsafe execution id must be rejected, got %v", err)
	}
}

func TestFormatDuration(t *testing.T) {
	if got := formatDuration(0); got != "-" {
		t.Fatalf("zero duration = %q, want -", got)
	}
	if got := formatDuration(int64(1500 * time.Millisecond / time.Millisecond)); got != "1.5s" {
		t.Fatalf("1500ms = %q, want 1.5s", got)
	}
}
