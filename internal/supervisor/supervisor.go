package supervisor

import (
	"context"
	"log"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/project"
)

// Tick is the reconcile cadence.
const Tick = 2 * time.Second

// Statuses the supervisor owns. "stopping", "draining" and "replacing" belong
// to the deploy engine and CLI commands and are never touched here beyond
// death finalization; "completed", "failed" and "crashloop" are terminal
// until a manual restart or a redeploy. A dead replica awaiting its backoff
// window sits in "restarting".
const (
	statusStarting   = "starting"
	statusRunning    = "running"
	statusRestarting = "restarting"
)

// replicaKey addresses one replica across the in-memory runtime maps.
type replicaKey struct {
	app  string
	slot string
}

// handleEntry tracks a replica this process spawned: the Process handle plus
// a channel carrying its exit status once Wait returns.
type handleEntry struct {
	proc deploy.Process
	done chan error
}

// probeRun is one in-flight exec probe. A replica never has more than one.
type probeRun struct {
	done chan deploy.ProbeOutcome
}

// Supervisor reconciles worker replicas toward the persisted desired state.
// One instance runs in the monitor daemon and owns every worker app; the
// zero value plus the runtime maps are cheap to build so tests can drive
// tick() directly.
type Supervisor struct {
	coord deploy.ReplicaCoordinator
	// launch is the spawn seam; tests replace it to avoid real processes.
	launch func(appName string, state *deploy.DeployState, slot string) (deploy.Process, error)

	mu         sync.Mutex
	handles    map[replicaKey]*handleEntry
	probes     map[replicaKey]*probeRun
	probeFails map[replicaKey]int
	nextProbe  map[replicaKey]time.Time

	clock  func() time.Time
	rand01 func() float64
}

// New returns a supervisor with real clocks and randomness.
func New() *Supervisor {
	return &Supervisor{
		launch:     deploy.LaunchWorkerReplica,
		handles:    make(map[replicaKey]*handleEntry),
		probes:     make(map[replicaKey]*probeRun),
		probeFails: make(map[replicaKey]int),
		nextProbe:  make(map[replicaKey]time.Time),
		clock:      time.Now,
		rand01:     rand.Float64,
	}
}

// Run reconciles every Tick until ctx is cancelled. Apps are reconciled in
// parallel so a long graceful stop on one app never delays the others.
func (s *Supervisor) Run(ctx context.Context) {
	s.tick()
	ticker := time.NewTicker(Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

func (s *Supervisor) tick() {
	// Read-only discovery: the monitor daemon's other goroutines mutate the
	// shared AppManager, so the supervisor never calls LoadState. Worker
	// apps come from the deploy-state directory scan, project directories
	// from a read-only apps.json parse.
	type workerApp struct{ name, dir string }
	var apps []workerApp
	dirs := app.AppDirectories()
	for _, name := range deploy.WorkerAppNames() {
		apps = append(apps, workerApp{name, dirs[name]})
	}
	var wg sync.WaitGroup
	for _, a := range apps {
		wg.Add(1)
		go func(name, dir string) {
			defer wg.Done()
			s.reconcileApp(name, dir)
		}(a.name, a.dir)
	}
	wg.Wait()
}

// reconcileApp performs one pass for one app under the replica coordinator
// lock. A busy lock means a deploy, scale or manual command owns the app's
// replicas: the pass is skipped, never queued.
func (s *Supervisor) reconcileApp(name, dir string) {
	release, busy, err := s.coord.TryLockApp(name)
	if err != nil {
		log.Printf("supervisor: %s: lock error: %v", name, err)
		return
	}
	if busy {
		return
	}
	defer release()

	state, err := deploy.Load(name)
	if err != nil || state == nil || state.Mode != deploy.ModeWorker {
		s.dropApp(name)
		return
	}
	cfg, err := project.Load(dir)
	if err != nil {
		log.Printf("supervisor: %s: config unreadable, skipping pass: %v", name, err)
		return
	}
	ps := cfg.Process.Resolve()
	hs := cfg.Health.ResolveWorker()
	sig, err := deploy.SignalFromName(ps.ShutdownSignal)
	if err != nil {
		sig, _ = deploy.SignalFromName(project.DefaultShutdownSignal)
	}

	dirty := false
	for _, slot := range sortedSlots(state.Replicas) {
		if s.reconcileReplica(name, dir, state, slot, ps, hs, sig) {
			dirty = true
		}
	}
	if dirty {
		if err := deploy.Store(state); err != nil {
			log.Printf("supervisor: %s: persist state: %v", name, err)
		}
	}
}

// reconcileReplica advances one replica's state machine. It returns true when
// the persisted record changed.
func (s *Supervisor) reconcileReplica(name, dir string, state *deploy.DeployState, slot string, ps project.ProcessSettings, hs project.WorkerHealthSettings, sig os.Signal) bool {
	inst := state.Replicas[slot]
	if inst == nil {
		return false
	}
	key := replicaKey{app: name, slot: slot}
	now := s.clock()

	waitErr, reaped := s.reapHandle(key)
	alive := !reaped && deploy.WorkerInstanceAlive(inst)

	switch inst.Status {
	case "stopping", "draining", "replacing":
		// Owned by a deploy, scale or manual command; only finalize a death.
		if alive {
			return false
		}
		s.recordExit(key, inst, waitErr, reaped)
		inst.Status = "stopped"
		s.dropHandle(key)
		s.clearProbes(key)
		return true

	case statusRestarting:
		if alive {
			inst.Status = statusStarting
			return true
		}
		due := inst.LastRestartAt.Add(s.restartDelay(inst, ps))
		if now.Before(due) {
			return false
		}
		proc, err := s.launch(name, state, slot)
		if err != nil {
			inst.LastExitCode = -1
			inst.LastExitReason = "relaunch failed: " + err.Error()
			inst.LastExitAt = now
			s.scheduleRestart(inst, ps, now)
			return true
		}
		s.setHandle(key, proc)
		return true

	case statusStarting:
		if !alive {
			s.finishExit(key, inst, ps, waitErr, reaped)
			return true
		}
		if hs.Mode != project.WorkerHealthExec {
			// process mode: observed liveness past the first tick is the
			// startup gate; a replica that dies later is a normal crash.
			inst.ReadyAt = now
			inst.Status = statusRunning
			return true
		}
		if now.Sub(inst.StartedAt) >= ps.StartupTimeout {
			_, waitErr, _ := s.stopLive(key, inst, sig, ps.GracePeriod)
			s.recordExit(key, inst, waitErr, !s.hasHandle(key))
			inst.LastExitReason = "startup timeout: " + inst.LastExitReason
			s.dropHandle(key)
			s.clearProbes(key)
			// The graceful stop usually exits 0, which the restart policy
			// would read as a completion; the timeout itself is the failure.
			s.scheduleRestart(inst, ps, s.clock())
			return true
		}
		if outcome, ok := s.probeOutcome(key); ok && outcome.OK {
			inst.ReadyAt = now
			inst.Status = statusRunning
			s.probeFails[key] = 0
			return true
		}
		s.maybeStartProbe(key, dir, state, inst, hs)
		return false

	case statusRunning:
		if !alive {
			s.finishExit(key, inst, ps, waitErr, reaped)
			return true
		}
		anchor := inst.ReadyAt
		if anchor.IsZero() {
			anchor = inst.StartedAt
		}
		if inst.RestartCount > 0 && now.Sub(anchor) >= project.StableWindow {
			inst.RestartCount = 0
			return true
		}
		if hs.Mode != project.WorkerHealthExec {
			return false
		}
		if outcome, ok := s.probeOutcome(key); ok {
			if outcome.OK {
				s.probeFails[key] = 0
			} else {
				s.probeFails[key]++
				if s.probeFails[key] >= hs.Retries {
					// retries failed probes count as one failure: one
					// health-driven restart, one attempt consumed.
					s.probeFails[key] = 0
					_, waitErr, _ := s.stopLive(key, inst, sig, ps.GracePeriod)
					s.recordExit(key, inst, waitErr, !s.hasHandle(key))
					inst.LastExitReason = "unhealthy: exec probe failed: " + outcome.Detail
					s.dropHandle(key)
					s.clearProbes(key)
					// The graceful stop usually exits 0, which the restart
					// policy would read as a completion; the health failure
					// itself is the decision input.
					s.scheduleRestart(inst, ps, s.clock())
					return true
				}
			}
		}
		s.maybeStartProbe(key, dir, state, inst, hs)
		return false

	default:
		// Terminal statuses (stopped, completed, failed, crashloop) and any
		// status written by another component: nothing to do.
		if !alive {
			s.dropHandle(key)
			s.clearProbes(key)
		}
		return false
	}
}

// finishExit records a dead replica's exit and applies the restart policy.
func (s *Supervisor) finishExit(key replicaKey, inst *deploy.Instance, ps project.ProcessSettings, waitErr error, reaped bool) {
	s.recordExit(key, inst, waitErr, reaped)
	s.clearProbes(key)
	s.applyPolicy(key, inst, ps)
}

// recordExit writes the exit bookkeeping: exact code when this process
// spawned the replica, the adopted marker otherwise.
func (s *Supervisor) recordExit(key replicaKey, inst *deploy.Instance, waitErr error, reaped bool) {
	s.dropHandle(key)
	if reaped {
		deploy.RecordInstanceExit(inst, waitErr, false)
		return
	}
	deploy.RecordInstanceExit(inst, nil, true)
}

// applyPolicy applies the restart policy to a recorded exit and either
// schedules a restart or moves the replica to a terminal status.
func (s *Supervisor) applyPolicy(key replicaKey, inst *deploy.Instance, ps project.ProcessSettings) {
	switch Evaluate(ps.RestartPolicy, inst.LastExitCode) {
	case DecisionComplete:
		inst.Status = "completed"
	case DecisionFail:
		inst.Status = "failed"
	default:
		s.scheduleRestart(inst, ps, s.clock())
	}
}

// scheduleRestart arms the backoff for another launch. Once the consecutive
// counter reaches max_attempts the replica enters crashloop and stays there
// until a manual restart or redeploy resets it.
func (s *Supervisor) scheduleRestart(inst *deploy.Instance, ps project.ProcessSettings, now time.Time) {
	if ps.MaxRestartAttempts > 0 && inst.RestartCount >= ps.MaxRestartAttempts {
		inst.Status = "crashloop"
		return
	}
	inst.RestartCount++
	inst.LastRestartAt = now
	inst.Status = statusRestarting
}

// restartDelay re-derives the remaining wait for a replica in status
// restarting: the backoff of its last scheduled attempt, plus fresh jitter so
// a fleet of replicas dying together does not restart in lockstep.
func (s *Supervisor) restartDelay(inst *deploy.Instance, ps project.ProcessSettings) time.Duration {
	attempt := inst.RestartCount - 1
	if attempt < 0 {
		attempt = 0
	}
	d := Backoff(ps.BackoffInitial, ps.BackoffMax, ps.BackoffMultiplier, attempt)
	return ApplyJitter(d, s.rand01()*2*JitterFactor-JitterFactor)
}

// stopLive gracefully stops a live replica, using the supervisor's handle
// when it has one and adopting the process otherwise.
func (s *Supervisor) stopLive(key replicaKey, inst *deploy.Instance, sig os.Signal, grace time.Duration) (deploy.ShutdownReport, error, error) {
	var held deploy.Process
	if h := s.handle(key); h != nil {
		held = h.proc
	}
	return deploy.GracefulStopWorker(inst, sig, grace, held)
}

// maybeStartProbe launches the replica's exec probe when none is in flight
// and the interval has elapsed.
func (s *Supervisor) maybeStartProbe(key replicaKey, dir string, state *deploy.DeployState, inst *deploy.Instance, hs project.WorkerHealthSettings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probes[key] != nil {
		return
	}
	now := s.clock()
	if next, ok := s.nextProbe[key]; ok && now.Before(next) {
		return
	}
	s.nextProbe[key] = now.Add(hs.Interval)
	env, err := deploy.EnvOverlayFromSnapshot(inst.EnvPath, state.AppID)
	if err != nil {
		env = nil
	}
	pr := &probeRun{done: make(chan deploy.ProbeOutcome, 1)}
	s.probes[key] = pr
	go func() { pr.done <- deploy.RunProbe(hs.Command, dir, env, hs.Timeout) }()
}

// probeOutcome collects a finished probe, if any.
func (s *Supervisor) probeOutcome(key replicaKey) (deploy.ProbeOutcome, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := s.probes[key]
	if pr == nil {
		return deploy.ProbeOutcome{}, false
	}
	select {
	case o := <-pr.done:
		delete(s.probes, key)
		return o, true
	default:
		return deploy.ProbeOutcome{}, false
	}
}

func (s *Supervisor) setHandle(key replicaKey, proc deploy.Process) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := &handleEntry{proc: proc, done: make(chan error, 1)}
	go func() { h.done <- proc.Wait() }()
	s.handles[key] = h
}

func (s *Supervisor) reapHandle(key replicaKey) (error, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.handles[key]
	if h == nil {
		return nil, false
	}
	select {
	case err := <-h.done:
		delete(s.handles, key)
		return err, true
	default:
		return nil, false
	}
}

func (s *Supervisor) handle(key replicaKey) *handleEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handles[key]
}

func (s *Supervisor) hasHandle(key replicaKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handles[key] != nil
}

func (s *Supervisor) dropHandle(key replicaKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.handles, key)
}

func (s *Supervisor) clearProbes(key replicaKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.probes, key)
	delete(s.probeFails, key)
	delete(s.nextProbe, key)
}

func (s *Supervisor) dropApp(appName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.handles {
		if key.app == appName {
			delete(s.handles, key)
		}
	}
	for key := range s.probes {
		if key.app == appName {
			delete(s.probes, key)
			delete(s.probeFails, key)
			delete(s.nextProbe, key)
		}
	}
}

// sortedSlots orders replica slots numerically ("2" before "10").
func sortedSlots(replicas map[string]*deploy.Instance) []string {
	keys := make([]int, 0, len(replicas))
	byKey := make(map[int]string, len(replicas))
	for slot := range replicas {
		n, err := strconv.Atoi(slot)
		if err != nil {
			n = -1
		}
		keys = append(keys, n)
		byKey[n] = slot
	}
	sort.Ints(keys)
	out := make([]string, 0, len(keys))
	for _, n := range keys {
		out = append(out, byKey[n])
	}
	return out
}
