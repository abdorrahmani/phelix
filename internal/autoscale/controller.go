package autoscale

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// AppRef is one deploy-managed app the controller evaluates. The monitor
// wires it from the app registry.
type AppRef struct {
	Name      string
	ID        string
	Directory string
}

// Controller runs the automatic autoscaling loop for the monitor daemon: one
// evaluation pass per enabled app at its configured interval, dispatching
// engine decisions to the existing scaling paths. It owns no replica
// lifecycle itself: worker decisions go through deploy.WorkerScale (which
// takes the app lock internally), rolling decisions through the
// ScaleExecutor (which takes the same lock). The controller therefore never
// takes the app lock before dispatch, and per-app single-flight plus the
// executor's stale-decision rejection make overlapping evaluations safe.
type Controller struct {
	// Apps lists the deploy-managed apps to consider each tick.
	Apps func() []AppRef
	// Proxy is the shared proxy control client; nil disables traffic metrics
	// for every app (workers never had them anyway).
	Proxy *proxy.Client
	// Now is the clock seam; tests inject a fake clock.
	Now func() time.Time
	// EvalTimeout bounds one app's full evaluation including any scaling
	// operation. The app lock alone can hold 30s, so this must exceed it.
	EvalTimeout time.Duration

	// sampleFn and dispatchFn are test seams; production leaves them nil and
	// uses the per-app sampler and the built-in dispatch.
	sampleFn   func(ctx context.Context, st *appState, ref AppRef, state *deploy.DeployState) deploy.AutoscaleMetrics
	dispatchFn func(ctx context.Context, ref AppRef, state *deploy.DeployState, d deploy.ScaleDecision) error

	mu       sync.Mutex
	states   map[string]*appState
	inFlight map[string]bool
}

type appState struct {
	sampler  *Sampler
	engine   *deploy.Autoscaler
	settings project.AutoscaleSettings
	lastEval time.Time
}

// NewController builds a controller. Production callers leave Now and
// EvalTimeout at their zero values.
func NewController(apps func() []AppRef, proxyClient *proxy.Client) *Controller {
	return &Controller{
		Apps:        apps,
		Proxy:       proxyClient,
		Now:         time.Now,
		EvalTimeout: 2 * time.Minute,
		states:      make(map[string]*appState),
		inFlight:    make(map[string]bool),
	}
}

// Run drives the evaluation loop until ctx is cancelled. The tick is one
// second so per-app intervals of a few seconds stay honest without one
// goroutine per app.
func (c *Controller) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.tick(ctx)
		}
	}
}

// tick starts at most one evaluation per eligible app.
func (c *Controller) tick(ctx context.Context) {
	if c.Apps == nil {
		return
	}
	now := c.Now()
	for _, ref := range c.Apps() {
		if ref.Name == "" {
			continue
		}
		c.mu.Lock()
		busy := c.inFlight[ref.Name]
		c.mu.Unlock()
		if busy {
			continue
		}
		st, ok := c.stateFor(ref)
		if !ok {
			continue
		}
		if now.Sub(st.lastEval) < st.settings.Interval {
			continue
		}
		c.mu.Lock()
		c.inFlight[ref.Name] = true
		c.mu.Unlock()
		st.lastEval = now
		go c.evaluateApp(ctx, ref)
	}
}

// stateFor returns the per-app evaluation state, creating the sampler and
// engine lazily and rebuilding the engine when the resolved settings change.
func (c *Controller) stateFor(ref AppRef) (*appState, bool) {
	settings, ok := effectiveSettings(ref)
	if !ok {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.states[ref.Name]
	if !ok {
		var stats ProxyStatsSource
		if c.Proxy != nil {
			stats = c.Proxy
		}
		st = &appState{sampler: NewSampler(ref.Name, stats)}
		c.states[ref.Name] = st
	}
	if st.engine == nil || st.settings != settings {
		st.engine = deploy.NewAutoscaler(settings)
		st.engine.SetClock(c.Now)
		st.settings = settings
	}
	return st, true
}

// effectiveSettings resolves the app's phelix.yaml autoscaling block.
// Apps without a config, without the block, or with autoscaling disabled
// return ok=false.
func effectiveSettings(ref AppRef) (project.AutoscaleSettings, bool) {
	if ref.Directory == "" {
		return project.AutoscaleSettings{}, false
	}
	cfg, err := project.Load(ref.Directory)
	if err != nil || cfg == nil || cfg.Deploy == nil || cfg.Deploy.Autoscaling == nil {
		return project.AutoscaleSettings{}, false
	}
	s := cfg.Deploy.Autoscaling.Resolve(cfg.Deploy.Replicas)
	if !s.Enabled {
		return project.AutoscaleSettings{}, false
	}
	return s, true
}

// evaluateApp runs one full evaluation for one app: enumerate, sample,
// decide, dispatch, record. Every scaling outcome lands in the history and
// in an ops record so `phelix operation status` and the history explain the
// fleet's shape.
func (c *Controller) evaluateApp(ctx context.Context, ref AppRef) {
	defer func() {
		c.mu.Lock()
		delete(c.inFlight, ref.Name)
		c.mu.Unlock()
	}()

	paused, err := LoadPauseState(ref.Name)
	if err == nil && paused.Paused {
		return
	}

	evalCtx, cancel := context.WithTimeout(ctx, c.EvalTimeout)
	defer cancel()

	state, err := deploy.Load(ref.Name)
	if err != nil {
		return
	}
	current := len(state.Replicas)

	st, ok := c.stateFor(ref)
	if !ok {
		return
	}
	snap := st.sampler.Collect(evalCtx, replicaRefs(state))
	decision := st.engine.Evaluate(snap.Metrics, current)
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
	execErr := c.dispatchTo(evalCtx, ref, state, decision)
	now := c.Now()
	switch {
	case execErr == nil:
		st.engine.ConfirmExecution(now)
		c.record(ref.Name, HistoryExecuted, decision, "")
		if record != nil {
			_ = record.MarkSucceeded(&ops.Result{Strategy: string(state.Mode)})
		}
	case phelixerr.CodeOf(execErr) == phelixerr.CodeDeployLocked:
		st.engine.AbortDecision()
		c.record(ref.Name, HistorySuppressed, decision, execErr.Error())
		if record != nil {
			_ = record.MarkFailed(execErr)
		}
	default:
		st.engine.AbortDecision()
		c.record(ref.Name, HistoryFailed, decision, execErr.Error())
		if record != nil {
			_ = record.MarkFailed(execErr)
		}
	}
}

// sample collects one observation through the per-app sampler. Workers and
// unenrolled apps see no applicable traffic source, so their resource-only
// samples fall out of the sampler's applicability rules.
func (c *Controller) sample(ctx context.Context, st *appState, ref AppRef, state *deploy.DeployState) deploy.AutoscaleMetrics {
	if c.sampleFn != nil {
		return c.sampleFn(ctx, st, ref, state)
	}
	var stats ProxyStatsSource
	if c.Proxy != nil && state.Mode == deploy.ModeRolling {
		stats = c.Proxy
	}
	st.sampler.SetStatsSource(stats)
	snap := st.sampler.Collect(ctx, replicaRefs(state))
	return snap.Metrics
}

// dispatchTo hands the decision to the app type's existing scaling path,
// through the test seam when one is installed.
func (c *Controller) dispatchTo(ctx context.Context, ref AppRef, state *deploy.DeployState, decision deploy.ScaleDecision) error {
	if c.dispatchFn != nil {
		return c.dispatchFn(ctx, ref, state, decision)
	}
	return c.dispatch(ctx, ref, state, decision)
}

// dispatch is the production execution path: worker decisions go through
// deploy.WorkerScale (which takes the app lock itself), rolling decisions
// through the ScaleExecutor (which takes the same lock). The controller
// never holds the lock across a dispatch.
func (c *Controller) dispatch(ctx context.Context, ref AppRef, state *deploy.DeployState, decision deploy.ScaleDecision) error {
	switch state.Mode {
	case deploy.ModeWorker:
		sig, grace, err := workerStopSettings(ref.Directory)
		if err != nil {
			return err
		}
		_, _, err = deploy.WorkerScale(ctx, ref.Name, decision.DesiredReplicas, sig, grace)
		return err
	case deploy.ModeRolling:
		ex := &deploy.ScaleExecutor{
			AppName:        ref.Name,
			AppID:          ref.ID,
			PublicPort:     state.PublicPort,
			Launcher:       deploy.LauncherForApp(ref.Name),
			ProxyClient:    c.Proxy,
			HealthProvider: deploy.DefaultHealthProvider(),
			GracePeriod:    time.Duration(state.GraceSeconds) * time.Second,
		}
		return ex.Scale(ctx, decision.CurrentReplicas, decision.DesiredReplicas)
	}
	return phelixerr.Newf(phelixerr.CodeConfiguration, "autoscale: mode %s has no scaling path", state.Mode)
}

// workerStopSettings resolves the shutdown signal and grace period from the
// app's phelix.yaml process block, matching the manual scale command's
// behavior. A missing config falls back to the documented defaults.
func workerStopSettings(dir string) (sig os.Signal, grace time.Duration, err error) {
	cfg, cfgErr := project.Load(dir)
	if cfgErr != nil && phelixerr.CodeOf(cfgErr) != phelixerr.CodeNotFound {
		return nil, 0, cfgErr
	}
	if cfg == nil {
		cfg = &project.Config{}
	}
	ps := cfg.Process.Resolve()
	sig, err = deploy.SignalFromName(ps.ShutdownSignal)
	if err != nil {
		return nil, 0, err
	}
	return sig, ps.GracePeriod, nil
}

func replicaRefs(state *deploy.DeployState) []ReplicaRef {
	refs := make([]ReplicaRef, 0, len(state.Replicas))
	for key, inst := range state.Replicas {
		if inst != nil && inst.PID > 0 {
			refs = append(refs, ReplicaRef{Key: key, PID: inst.PID})
		}
	}
	return refs
}

// record appends a history entry, never failing the evaluation.
func (c *Controller) record(app, kind string, d deploy.ScaleDecision, execErr string) {
	rec := HistoryRecord{
		App:       app,
		Kind:      kind,
		Direction: directionName(d.Direction),
		From:      d.CurrentReplicas,
		To:        d.DesiredReplicas,
		Reason:    d.Reason,
		Error:     execErr,
	}
	_ = AppendHistoryRecord(app, rec)
}

func directionName(d deploy.ScaleDirection) string {
	switch d {
	case deploy.ScaleUp:
		return "up"
	case deploy.ScaleDown:
		return "down"
	}
	return "none"
}
