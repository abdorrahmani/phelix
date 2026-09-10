package grpc

import (
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/builder"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/matrix"
)

// eventRecorder captures every emitted matrix event via the interceptor hook.
type eventRecorder struct {
	mu     sync.Mutex
	events []*pb.MatrixEvent
}

func recordMatrixEvents(t *testing.T) *eventRecorder {
	t.Helper()
	rec := &eventRecorder{}
	SetMatrixEventInterceptor(func(ev *pb.MatrixEvent) {
		rec.mu.Lock()
		rec.events = append(rec.events, ev)
		rec.mu.Unlock()
	})
	t.Cleanup(func() { SetMatrixEventInterceptor(nil) })
	return rec
}

func (r *eventRecorder) all() []*pb.MatrixEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*pb.MatrixEvent, len(r.events))
	copy(out, r.events)
	return out
}

func (r *eventRecorder) waitFor(t *testing.T, n int) []*pb.MatrixEvent {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		got := r.all()
		if len(got) >= n {
			return got
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %d events, got %d", n, len(got))
		}
	}
}

func TestMatrixReporter_NativeRunLifecycle(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	rec := recordMatrixEvents(t)

	prof := &matrix.Profile{Lang: builder.Go, Versions: []string{"1.22"}, Platforms: []string{"linux/amd64"}, Concurrency: 1}
	run := matrix.NewRun(matrix.RunID("mx_20260910_e704"), "myapp", "/tmp/p", prof, time.Now())
	run.InitCombinations([]matrix.Combination{
		{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
	})

	rep := NewMatrixReporter("req-42", string(run.ID), "myapp", MatrixModeNative)
	rep.RunStarted(run)
	combos := run.IncompleteCombinations()
	rep.AttemptStarted(combos[0], 1)
	rep.ResultRecorded(matrix.Result{
		Combination: combos[0],
		Status:      "success",
		Duration:    time.Second,
		Attempts:    []matrix.Attempt{{Number: 1, Status: "success", Duration: time.Second}},
	})
	run.RecordResult(matrix.Result{Combination: combos[0], Status: "success", Attempts: []matrix.Attempt{{Number: 1, Status: "success"}}})
	run.Finalize(time.Now())
	rep.RunFinished(run, false)

	events := rec.waitFor(t, 4)
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4", len(events))
	}

	// Every event carries the correlation identity.
	for i, ev := range events {
		if ev.GetRequestId() != "req-42" || ev.GetMatrixRunId() != "mx_20260910_e704" ||
			ev.GetAppName() != "myapp" || ev.GetMode() != MatrixModeNative {
			t.Fatalf("event %d identity: %+v", i, ev)
		}
		if ev.GetSeq() != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d (monotonic from 1)", i, ev.GetSeq(), i+1)
		}
		if ev.GetTimestamp() == 0 || ev.GetServerId() == "" {
			t.Fatalf("event %d missing timestamp/server_id", i)
		}
	}

	if events[0].GetEvent() != MatrixEventStarted || events[0].GetRun() == nil {
		t.Fatalf("first event: %+v", events[0])
	}
	if events[0].GetStatus() != string(matrix.RunStatusRunning) {
		t.Fatalf("started status: %s", events[0].GetStatus())
	}
	if events[0].GetCounters().GetTotal() != 1 || events[0].GetCounters().GetPending() != 1 {
		t.Fatalf("started counters: %+v", events[0].GetCounters())
	}

	if events[1].GetEvent() != MatrixEventCombinationStarted || events[1].GetAttempt() != 1 {
		t.Fatalf("combination_started: %+v", events[1])
	}
	if events[1].GetCombination().GetId() != "go1.22-linux-amd64" {
		t.Fatalf("combination identity: %+v", events[1].GetCombination())
	}

	if events[2].GetEvent() != MatrixEventCombinationCompleted || events[2].GetCombination().GetStatus() != "success" {
		t.Fatalf("combination_completed: %+v", events[2])
	}
	if events[2].GetCombination().GetAttempts() != 1 {
		t.Fatalf("completed attempts: %+v", events[2].GetCombination())
	}

	if events[3].GetEvent() != MatrixEventCompleted || events[3].GetStatus() != string(matrix.RunStatusSucceeded) {
		t.Fatalf("terminal event: %+v", events[3])
	}
	if events[3].GetRun() == nil || events[3].GetRun().GetCounters().GetSucceeded() != 1 {
		t.Fatalf("terminal run state: %+v", events[3].GetRun())
	}
}

func TestMatrixReporter_ResumedAndInterrupted(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	rec := recordMatrixEvents(t)

	prof := &matrix.Profile{Lang: builder.Go, Versions: []string{"1.22"}, Platforms: []string{"linux/amd64"}, Concurrency: 1}
	run := matrix.NewRun(matrix.RunID("mx_20260910_e705"), "myapp", "/tmp/p", prof, time.Now())
	run.InitCombinations([]matrix.Combination{
		{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
	})
	run.ResumeCount = 1
	run.Status = matrix.RunStatusInterrupted

	rep := NewMatrixReporter("req-43", string(run.ID), "myapp", MatrixModeNative)
	rep.RunResumed(run)
	rep.RunFinished(run, true)

	events := rec.waitFor(t, 2)
	if events[0].GetEvent() != MatrixEventResumed {
		t.Fatalf("first event = %s, want matrix.resumed", events[0].GetEvent())
	}
	if events[0].GetRun() == nil || events[0].GetRun().GetResumeCount() != 1 {
		t.Fatalf("resumed run state: %+v", events[0].GetRun())
	}
	if events[1].GetEvent() != MatrixEventInterrupted || events[1].GetStatus() != string(matrix.RunStatusInterrupted) {
		t.Fatalf("terminal event: %+v", events[1])
	}
}

func TestMatrixReporter_DockerLifecycle(t *testing.T) {
	rec := recordMatrixEvents(t)

	rep := NewMatrixReporter("req-44", "", "myapp", MatrixModeDocker)
	plan := &matrix.MatrixPlan{
		Lang:         builder.Go,
		Combinations: []matrix.Combination{{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}},
	}
	rep.DockerStarted(plan, map[string]string{"registry": "ghcr.io", "tag": "v1", "push": "false"})
	rep.AttemptStarted(plan.Combinations[0], 1)
	rep.ResultRecorded(matrix.Result{
		Combination: plan.Combinations[0],
		Status:      "failed",
		Error:       errBuild("docker build failed"),
		Attempts:    []matrix.Attempt{{Number: 1, Status: "failed", Error: errBuild("docker build failed")}},
	})
	rep.DockerFinished([]matrix.Result{{
		Combination: plan.Combinations[0],
		Status:      "failed",
		Attempts:    []matrix.Attempt{{Number: 1, Status: "failed"}},
	}})

	events := rec.waitFor(t, 4)
	for i, ev := range events {
		if ev.GetMode() != MatrixModeDocker || ev.GetMatrixRunId() != "" || ev.GetRequestId() != "req-44" {
			t.Fatalf("event %d identity: %+v", i, ev)
		}
		if ev.GetSeq() != int64(i+1) {
			t.Fatalf("event %d seq = %d", i, ev.GetSeq())
		}
	}
	if events[0].GetEvent() != MatrixEventStarted || events[0].GetMetadata()["registry"] != "ghcr.io" {
		t.Fatalf("docker started: %+v", events[0])
	}
	if events[0].GetCounters().GetTotal() != 1 || events[0].GetCounters().GetPending() != 1 {
		t.Fatalf("docker started counters: %+v", events[0].GetCounters())
	}
	// All combinations failed → aggregate status "failed" on the terminal
	// event (the same derivation a run applies).
	last := events[3]
	if last.GetEvent() != MatrixEventCompleted || last.GetStatus() != "failed" {
		t.Fatalf("docker terminal: %+v", last)
	}
	if last.GetCounters().GetFailed() != 1 || last.GetCounters().GetTotal() != 1 {
		t.Fatalf("docker terminal counters: %+v", last.GetCounters())
	}
}

func TestMatrixReporter_NilReceiverIsSafe(t *testing.T) {
	// A nil reporter (handlers must be able to pass one unconditionally).
	var rep *MatrixReporter
	rep.RunStarted(nil)
	rep.AttemptStarted(matrix.Combination{}, 1)
	rep.ResultRecorded(matrix.Result{})
	rep.RunFinished(nil, false)
	rep.DockerStarted(nil, nil)
	rep.DockerFinished(nil)
}
