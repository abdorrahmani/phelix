package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/job"
)

func TestRemoteJobCommandFlow(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dataDir)
	t.Setenv("HOME", t.TempDir())
	dir := writeJobProject(t, "echoes", "name: echoes\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\njob:\n  command: [\"sh\", \"-c\", \"echo remote-out\"]\n")
	seedJobApp(t, "echoes", dir)

	list := RemoteJobCommand(&pb.MonitorCommandRequest{RequestId: "r1", Type: phelixgrpc.JobCommandList, AppName: "echoes"})
	if list.GetStatus() != "success" || len(list.GetJobs()) != 1 {
		t.Fatalf("list = %+v", list)
	}
	if job := list.GetJobs()[0]; job.GetCron() != "0 3 * * *" || job.GetAppId() != "app-echoes" || job.GetCurrentStatus() != "idle" {
		t.Fatalf("definition = %+v", job)
	}

	run := RemoteJobCommand(&pb.MonitorCommandRequest{RequestId: "r2", Type: phelixgrpc.JobCommandRun, AppName: "echoes"})
	if run.GetStatus() != "success" || run.GetExecution() == nil {
		t.Fatalf("run = %+v", run)
	}
	executionID := run.GetExecution().GetExecutionId()
	if executionID == "" || run.GetExecution().GetTrigger() != "manual" {
		t.Fatalf("execution = %+v", run.GetExecution())
	}

	var record *job.Record
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		record, _ = job.NewStore(dataDir).Load("echoes", executionID)
		if record != nil && record.Status.Finished() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if record == nil || record.Status != job.StatusSucceeded {
		t.Fatalf("record = %+v", record)
	}

	history := RemoteJobCommand(&pb.MonitorCommandRequest{
		RequestId: "r3", Type: phelixgrpc.JobCommandHistory, AppName: "echoes",
		Job: &pb.JobOptions{Limit: 5},
	})
	if history.GetStatus() != "success" || len(history.GetExecutions()) != 1 {
		t.Fatalf("history = %+v", history)
	}
	if history.GetExecutions()[0].GetExecutionId() != executionID || history.GetExecutions()[0].GetStatus() != "succeeded" {
		t.Fatalf("history record = %+v", history.GetExecutions()[0])
	}

	logs := RemoteJobCommand(&pb.MonitorCommandRequest{
		RequestId: "r4", Type: phelixgrpc.JobCommandLogs, AppName: "echoes",
		Job: &pb.JobOptions{ExecutionId: executionID},
	})
	if logs.GetStatus() != "success" || logs.GetJobLog() == nil {
		t.Fatalf("logs = %+v", logs)
	}
	found := false
	for _, entry := range logs.GetJobLog().GetEntries() {
		if entry.GetMessage() == "remote-out" {
			found = true
			if entry.GetStream() != "stdout" || entry.GetLevel() != "info" {
				t.Fatalf("log entry = %+v", entry)
			}
		}
	}
	if !found {
		t.Fatalf("logs = %+v", logs.GetJobLog())
	}
}

func TestRemoteJobCommandRejectsWrongTarget(t *testing.T) {
	serviceDir := writeJobProject(t, "web", "name: web\nport: 8080\n")
	jobDir := writeJobProject(t, "nightly", "name: nightly\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\njob:\n  command: [\"true\"]\n")
	withJobManager(t, map[string]*app.AppInfo{
		"app-web":     {ID: "app-web", Name: "web", Directory: serviceDir},
		"app-nightly": {ID: "app-nightly", Name: "nightly", Directory: jobDir},
	})

	service := RemoteJobCommand(&pb.MonitorCommandRequest{RequestId: "r1", Type: phelixgrpc.JobCommandList, AppName: "web"})
	if service.GetStatus() != "error" || service.GetErrorCode() != string(phelixerr.CodeInvalidArgument) {
		t.Fatalf("service target = %+v", service)
	}

	mismatch := RemoteJobCommand(&pb.MonitorCommandRequest{
		RequestId: "r2", Type: phelixgrpc.JobCommandRun, AppName: "nightly",
		Job: &pb.JobOptions{JobName: "other"},
	})
	if mismatch.GetStatus() != "error" || mismatch.GetErrorCode() != string(phelixerr.CodeInvalidArgument) {
		t.Fatalf("job name mismatch = %+v", mismatch)
	}

	missingLogs := RemoteJobCommand(&pb.MonitorCommandRequest{
		RequestId: "r3", Type: phelixgrpc.JobCommandLogs, AppName: "nightly",
		Job: &pb.JobOptions{ExecutionId: "job_0123456789abcdef"},
	})
	if missingLogs.GetStatus() != "error" || missingLogs.GetErrorCode() != string(phelixerr.CodeNotFound) {
		t.Fatalf("missing execution = %+v", missingLogs)
	}
}

func TestParseJobLogLine_PreservesStreamAndLevel(t *testing.T) {
	entry := parseJobLogLine("2026/01/02 03:04:05 [INFO] [stdout] hello world")
	if entry.Level != "INFO" || entry.Stream != "stdout" || entry.Message != "hello world" {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.Timestamp == 0 {
		t.Fatal("timestamp was not parsed")
	}

	errEntry := parseJobLogLine("2026/01/02 03:04:06 [ERROR] [stderr] boom")
	if errEntry.Level != "ERROR" || errEntry.Stream != "stderr" || errEntry.Message != "boom" {
		t.Fatalf("stderr entry = %+v", errEntry)
	}

	raw := parseJobLogLine("plain output")
	if raw.Message != "plain output" || raw.Stream != "" || raw.Timestamp != 0 {
		t.Fatalf("raw entry = %+v", raw)
	}
}

func TestReadJobLog_PagesOnLineBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job_0123456789abcdef.log")
	lines := []string{
		"2026/01/02 03:04:05 [INFO] [stdout] first",
		"2026/01/02 03:04:06 [ERROR] [stderr] second",
		"2026/01/02 03:04:07 [INFO] [stdout] third",
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	var messages []string
	offset := int64(0)
	for page := 0; page < 10; page++ {
		chunk, err := readJobLog(path, "job_0123456789abcdef", offset, 70)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if chunk.Offset != offset {
			t.Fatalf("page %d offset = %d, want %d", page, chunk.Offset, offset)
		}
		for _, entry := range chunk.Entries {
			messages = append(messages, entry.Message)
		}
		if chunk.Eof {
			break
		}
		if chunk.NextOffset <= offset {
			t.Fatalf("page %d did not advance: %d", page, chunk.NextOffset)
		}
		offset = chunk.NextOffset
	}
	if got := strings.Join(messages, ","); got != "first,second,third" {
		t.Fatalf("messages = %q", got)
	}
}

func TestReadJobLog_RejectsLineLargerThanPage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job_0123456789abcdef.log")
	long := strings.Repeat("x", 200)
	if err := os.WriteFile(path, []byte(long+"\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if _, err := readJobLog(path, "job_0123456789abcdef", 0, 50); err == nil {
		t.Fatal("a line larger than max_bytes must be rejected instead of splitting it")
	}
}

func TestReadJobLog_EmptyAndOffsetAtEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job_0123456789abcdef.log")
	if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	chunk, err := readJobLog(path, "job_0123456789abcdef", 4, 0)
	if err != nil {
		t.Fatalf("readJobLog: %v", err)
	}
	if !chunk.Eof || chunk.TotalBytes != 4 || chunk.NextOffset != 4 {
		t.Fatalf("chunk = %+v", chunk)
	}
	missing, err := readJobLog(filepath.Join(t.TempDir(), "gone.log"), "job_0123456789abcdef", 0, 0)
	if err != nil {
		t.Fatalf("missing log: %v", err)
	}
	if !missing.Eof || missing.TotalBytes != 0 {
		t.Fatalf("missing chunk = %+v", missing)
	}
}
