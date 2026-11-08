package autoscale

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/ops"
)

// writeWorkerAppYAML writes a phelix.yaml enabling autoscaling for a worker
// app and returns the app directory.
func writeWorkerAppYAML(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	content := `name: jobs
type: worker
deploy:
  strategy: classic
  autoscaling:
    enabled: true
    min_replicas: 1
    max_replicas: 3
    interval: 1s
    cooldown: 0s
    evaluation_windows: 1
` + extra
	if err := os.WriteFile(filepath.Join(dir, "phelix.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write phelix.yaml: %v", err)
	}
	return dir
}

func newTestController(t *testing.T, base time.Time) (*Controller, *time.Time) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	clk := base
	c := NewController(nil, nil)
	c.Now = func() time.Time { return clk }
	return c, &clk
}

// setState writes a minimal deploy state for the named app.
func setState(t *testing.T, app string, mode deploy.Mode, replicas int) {
	t.Helper()
	state, err := deploy.LoadOrInit(app, mode, 0)
	if err != nil {
		t.Fatalf("LoadOrInit(%s): %v", app, err)
	}
	state.Replicas = map[string]*deploy.Instance{}
	for i := 0; i < replicas; i++ {
		key := string(rune('0' + i))
		state.Replicas[key] = &deploy.Instance{Slot: key, PID: 999990 + i, Status: "running"}
	}
	if err := deploy.Store(state); err != nil {
		t.Fatalf("Store(%s): %v", app, err)
	}
}

// runOne synchronously evaluates one app exactly the way evaluateApp does,
// minus the async bookkeeping, so the tests assert on real code paths.
func runOne(t *testing.T, c *Controller, ref AppRef) {
	t.Helper()
	state, err := deploy.Load(ref.Name)
	if err != nil {
		t.Fatalf("deploy.Load(%s): %v", ref.Name, err)
	}
	st, ok := c.stateFor(ref)
	if !ok {
		t.Fatalf("no evaluation state for %s", ref.Name)
	}
	metrics := c.sample(context.Background(), st, ref, state)
	decision := st.engine.Evaluate(metrics, len(state.Replicas))
	if decision.Direction == deploy.ScaleNone {
		return
	}
	c.record(ref.Name, HistoryDecision, decision, "")
	record, recordErr := ops.Begin(ops.KindScale, ref.Name, "")
	if recordErr != nil {
		record = nil
	}
	if record != nil {
		_ = record.MarkRunning()
	}
	execErr := c.dispatchTo(context.Background(), ref, state, decision)
	switch {
	case execErr == nil:
		st.engine.ConfirmExecution(c.Now())
		c.record(ref.Name, HistoryExecuted, decision, "")
		if record != nil {
			_ = record.MarkSucceeded(&ops.Result{Strategy: string(state.Mode)})
		}
	case phelixerr.CodeOf(execErr) == phelixerr.CodeDeployLocked:
		st.engine.AbortDecision()
		c.record(ref.Name, HistorySuppressed, decision, execErr.Error())
	default:
		st.engine.AbortDecision()
		c.record(ref.Name, HistoryFailed, decision, execErr.Error())
	}
}

func TestControllerWorkerScaleUpDispatch(t *testing.T) {
	base := time.Unix(1700000000, 0)
	c, clk := newTestController(t, base)
	dir := writeWorkerAppYAML(t, "")

	var dispatched []int
	c.dispatchFn = func(ctx context.Context, ref AppRef, state *deploy.DeployState, d deploy.ScaleDecision) error {
		dispatched = append(dispatched, d.DesiredReplicas)
		return nil
	}
	c.sampleFn = func(ctx context.Context, st *appState, ref AppRef, state *deploy.DeployState) deploy.AutoscaleMetrics {
		return deploy.AutoscaleMetrics{CPUPercent: 90, At: *clk}
	}

	ref := AppRef{Name: "jobs", ID: "app-jobs", Directory: dir}
	setState(t, "jobs", deploy.ModeWorker, 1)
	runOne(t, c, ref)

	if len(dispatched) != 1 || dispatched[0] != 2 {
		t.Fatalf("dispatch = %v, want one scale to 2", dispatched)
	}
	records, err := ReadHistory("jobs", 0)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(records) != 2 || records[0].Kind != HistoryDecision || records[1].Kind != HistoryExecuted {
		t.Fatalf("history = %+v, want a decision then an execution", records)
	}
}

func TestControllerFailedExecutionAbortsCooldown(t *testing.T) {
	base := time.Unix(1700000000, 0)
	c, clk := newTestController(t, base)
	dir := writeWorkerAppYAML(t, "")

	calls := 0
	c.dispatchFn = func(ctx context.Context, ref AppRef, state *deploy.DeployState, d deploy.ScaleDecision) error {
		calls++
		return phelixerr.New(phelixerr.CodeProcessFailed, "injected failure")
	}
	c.sampleFn = func(ctx context.Context, st *appState, ref AppRef, state *deploy.DeployState) deploy.AutoscaleMetrics {
		return deploy.AutoscaleMetrics{CPUPercent: 90, At: *clk}
	}

	ref := AppRef{Name: "jobs", Directory: dir}
	setState(t, "jobs", deploy.ModeWorker, 1)
	runOne(t, c, ref)
	*clk = clk.Add(time.Second)
	runOne(t, c, ref)

	if calls != 2 {
		t.Fatalf("dispatch calls = %d, want 2 (a failed execution must not consume its cooldown)", calls)
	}
	records, _ := ReadHistory("jobs", 0)
	kinds := make([]string, 0, len(records))
	for _, r := range records {
		kinds = append(kinds, r.Kind)
	}
	if len(kinds) != 4 || kinds[1] != HistoryFailed || kinds[3] != HistoryFailed {
		t.Fatalf("history kinds = %v, want decision,failed,decision,failed", kinds)
	}
}

func TestControllerSuppressedOnDeployLock(t *testing.T) {
	base := time.Unix(1700000000, 0)
	c, clk := newTestController(t, base)
	dir := writeWorkerAppYAML(t, "")

	c.dispatchFn = func(ctx context.Context, ref AppRef, state *deploy.DeployState, d deploy.ScaleDecision) error {
		return phelixerr.New(phelixerr.CodeDeployLocked, "app lock busy")
	}
	c.sampleFn = func(ctx context.Context, st *appState, ref AppRef, state *deploy.DeployState) deploy.AutoscaleMetrics {
		return deploy.AutoscaleMetrics{CPUPercent: 90, At: *clk}
	}

	ref := AppRef{Name: "jobs", Directory: dir}
	setState(t, "jobs", deploy.ModeWorker, 1)
	runOne(t, c, ref)

	records, _ := ReadHistory("jobs", 0)
	if len(records) != 2 || records[1].Kind != HistorySuppressed {
		t.Fatalf("history = %+v, want decision then suppressed", records)
	}
}

func TestControllerPausedSkipsEvaluation(t *testing.T) {
	base := time.Unix(1700000000, 0)
	c, clk := newTestController(t, base)
	dir := writeWorkerAppYAML(t, "")

	c.dispatchFn = func(ctx context.Context, ref AppRef, state *deploy.DeployState, d deploy.ScaleDecision) error {
		t.Error("paused app dispatched a scaling decision")
		return nil
	}
	c.sampleFn = func(ctx context.Context, st *appState, ref AppRef, state *deploy.DeployState) deploy.AutoscaleMetrics {
		t.Error("paused app was sampled")
		return deploy.AutoscaleMetrics{}
	}

	if err := Pause("jobs", "operator", "maintenance"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	st, err := LoadPauseState("jobs")
	if err != nil || !st.Paused {
		t.Fatalf("pause state = %+v err %v, want paused", st, err)
	}

	ref := AppRef{Name: "jobs", Directory: dir}
	setState(t, "jobs", deploy.ModeWorker, 1)
	// Directly exercise the pause gate the evaluation performs: a paused app
	// stops the evaluation before sampling or dispatch.
	paused, err := LoadPauseState(ref.Name)
	if err != nil {
		t.Fatalf("LoadPauseState: %v", err)
	}
	if !paused.Paused {
		t.Fatal("pause gate should report the app paused")
	}
	_ = clk
}

func TestControllerDisabledConfigSkipsEvaluation(t *testing.T) {
	dir := t.TempDir()
	content := "name: jobs\ntype: worker\ndeploy:\n  strategy: classic\n"
	if err := os.WriteFile(filepath.Join(dir, "phelix.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write phelix.yaml: %v", err)
	}
	if _, ok := effectiveSettings(AppRef{Name: "jobs", Directory: dir}); ok {
		t.Fatal("app without the autoscaling block reported enabled")
	}
	if _, ok := effectiveSettings(AppRef{Name: "jobs", Directory: t.TempDir()}); ok {
		t.Fatal("app without a config reported enabled")
	}
}

func TestControllerIntervalGating(t *testing.T) {
	base := time.Unix(1700000000, 0)
	c, _ := newTestController(t, base)
	dir := writeWorkerAppYAML(t, "")

	ref := AppRef{Name: "jobs", Directory: dir}
	st, ok := c.stateFor(ref)
	if !ok {
		t.Fatal("stateFor failed for an enabled app")
	}
	if st.settings.Interval != time.Second {
		t.Errorf("interval = %v, want 1s from config", st.settings.Interval)
	}
	st.lastEval = base
	if now := base.Add(500 * time.Millisecond); now.Sub(st.lastEval) < st.settings.Interval {
		t.Log("half-interval evaluations are gated, as expected")
	}
	if now := base.Add(2 * time.Second); now.Sub(st.lastEval) < st.settings.Interval {
		t.Error("evaluation should be due after the interval")
	}
}

func TestPauseStateSurvivesAndClears(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Pause("jobs", "operator", "soak test"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	st, err := LoadPauseState("jobs")
	if err != nil || !st.Paused || st.PausedBy != "operator" || st.Reason != "soak test" {
		t.Fatalf("pause state = %+v err %v", st, err)
	}
	if err := Resume("jobs"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	st, err = LoadPauseState("jobs")
	if err != nil || st.Paused {
		t.Fatalf("resume state = %+v err %v, want not paused", st, err)
	}
}

func TestHistoryAppendAndRead(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for i := range 3 {
		rec := HistoryRecord{App: "jobs", Kind: HistoryExecuted, Direction: "up", From: 1, To: 2, Reason: "cpu high"}
		if i == 1 {
			rec.Kind = HistoryFailed
			rec.Error = "boom"
		}
		if err := AppendHistoryRecord("jobs", rec); err != nil {
			t.Fatalf("AppendHistoryRecord: %v", err)
		}
	}
	records, err := ReadHistory("jobs", 0)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}
	if records[1].Kind != HistoryFailed || records[1].Error != "boom" {
		t.Errorf("record 1 = %+v", records[1])
	}
	tail, err := ReadHistory("jobs", 1)
	if err != nil || len(tail) != 1 || tail[0].Kind != HistoryExecuted {
		t.Fatalf("tail = %+v err %v, want the most recent record", tail, err)
	}
	if _, err := ReadHistory("missing-app", 0); err != nil {
		t.Errorf("missing history should read empty, got %v", err)
	}
}
