package deploy

import (
	"context"
	"os"
	"sort"
	"strconv"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/port"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// This file owns the lifecycle operations on an existing DeployState —
// verifying reality (InstanceAlive/ServingInstance), tearing a deployment
// down (stop) and restoring it (start). It exists so `phelix stop/start/
// restart` can operate on proxy-managed apps without falling back to the
// classic single-PID flow, which would bind the public port directly and
// fight the proxy for ownership.

// stopRetiredInstances drains instances left over from a previous deployment
// strategy (blue-green slots during a migration to rolling, replicas during a
// migration to blue-green). The caller must only invoke it once the proxy no
// longer routes to them — i.e. after the new strategy's first successful
// membership update. Processes are verified by executable path before being
// signalled, so a stale record can never kill an unrelated process.
func stopRetiredInstances(ctx context.Context, retired []*Instance, grace time.Duration, inFlight int64, log Logger) {
	if log == nil {
		log = &nopLogger{}
	}
	for _, inst := range retired {
		// Docker instances record PID 0; their identity is the ContainerID, so a
		// PID<=0 skip would leak a retired container across a strategy migration.
		if inst == nil || (inst.PID <= 0 && inst.ContainerID == "") {
			continue
		}
		log.Stepf("stopping retired %q instance (%s) from previous strategy", inst.Slot, instanceRef(inst))
		report := stopInstance(ctx, inst, grace, inFlight)
		if report.ForceKilled {
			log.Warnf("retired instance %q ignored SIGTERM; SIGKILL applied", inst.Slot)
		}
		inst.Status = "stopped"
		inst.PID = 0
	}
}

// InstanceAlive reports whether inst's recorded process is alive AND still
// resolves to the identity recorded for it. For native instances the
// executable check makes a recycled PID unusable as "the instance is running"
// evidence; for docker instances (ContainerID set) the check routes through
// Docker and re-verifies the phelix.managed label. See instanceAliveViaRuntime.
func InstanceAlive(inst *Instance) bool {
	return instanceAliveViaRuntime(inst)
}

// ServingInstance returns the instance that should be serving traffic right
// now: the active slot for blue-green, or the first live replica for rolling.
// It returns nil when nothing is (or should be) serving.
func (s *DeployState) ServingInstance() *Instance {
	if s == nil {
		return nil
	}
	switch s.Mode {
	case ModeBlueGreen:
		return s.ActiveInstance()
	case ModeRolling:
		keys := sortedReplicaKeys(s.Replicas)
		for _, k := range keys {
			if inst := s.Replicas[k]; inst != nil && InstanceAlive(inst) {
				return inst
			}
		}
	}
	return nil
}

func sortedReplicaKeys(m map[string]*Instance) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, aerr := strconv.Atoi(keys[i])
		b, berr := strconv.Atoi(keys[j])
		if aerr == nil && berr == nil {
			return a < b
		}
		return keys[i] < keys[j]
	})
	return keys
}

// LifecycleDecision is the application lifecycle status derived from
// deployment reality rather than from what deploy.json claims.
type LifecycleDecision struct {
	// Status is "running" when every instance deploy.json claims to serve
	// traffic is alive, and "degraded" when at least one claimed instance is
	// alive but at least one is dead. Only "degraded" is persisted over the
	// stored record: a "stopped" reading is almost always this process's own
	// stale view (state written before this deploy swapped instances), and
	// stamping it would corrupt a concurrent deploy's fresh state — the exact
	// mechanism behind the "list says stopped while replicas run" bug.
	Status string
	// Alive / Desired count the instances the state claims to serve.
	Alive   int
	Desired int
	// PID is the serving process id (first alive instance; 0 when none).
	PID int
	// StartedAt is when the serving instance was launched (zero when unknown).
	StartedAt time.Time
	// PublicPort is the proxy-owned public port (0 when unknown).
	PublicPort int
}

// instancesServing returns the instance records that the state claims are
// serving traffic right now: the active slot for blue-green, or every recorded
// replica for rolling. Their liveness decides the lifecycle status.
func instancesServing(s *DeployState) []*Instance {
	if s == nil {
		return nil
	}
	switch s.Mode {
	case ModeBlueGreen:
		// An instance is "claimed to serve" when it has a live PID (native) OR a
		// ContainerID (docker instances record PID 0 — their identity is the
		// container). Liveness is then decided by InstanceAlive, which is already
		// runtime-aware. Gating on PID>0 alone excluded every docker instance and
		// reported a live container as stopped.
		if inst := s.ActiveInstance(); inst != nil && (inst.PID > 0 || inst.ContainerID != "") {
			return []*Instance{inst}
		}
	case ModeRolling:
		var out []*Instance
		for _, k := range sortedReplicaKeys(s.Replicas) {
			if inst := s.Replicas[k]; inst != nil && (inst.PID > 0 || inst.ContainerID != "") {
				out = append(out, inst)
			}
		}
		return out
	}
	return nil
}

// DecideLifecycle derives LifecycleDecision from a DeployState. Callers use
// it to keep the application lifecycle record consistent with the deployment
// instead of maintaining two independent, drifting notions of "running".
func DecideLifecycle(state *DeployState) LifecycleDecision {
	if state == nil {
		return LifecycleDecision{}
	}
	serving := instancesServing(state)
	d := LifecycleDecision{Desired: len(serving), PublicPort: state.PublicPort}
	for _, inst := range serving {
		if !InstanceAlive(inst) {
			continue
		}
		if d.PID == 0 {
			d.PID = inst.PID
			d.StartedAt = inst.StartedAt
		}
		d.Alive++
	}
	switch {
	case d.Desired == 0 || d.Alive == 0:
		d.Status = "stopped"
	case d.Alive < d.Desired:
		d.Status = "degraded"
	default:
		d.Status = "running"
	}
	return d
}

// Reconcile rewrites the persisted runtime state so it describes reality, and
// reports what the lifecycle record should say:
//
//   - replicas/slots whose recorded process is gone or was recycled get their
//     PID cleared and status "stopped" — deploy.json can then never name a
//     PID that is not Phelix's (Step 9: stale-PID reconciliation).
//   - instance Status strings are corrected against liveness ("running" with
//     a dead PID is a lie; "stopped" with a live PID is a lie).
//   - the state file is re-persisted only when something changed.
//
// It never fabricates a running status: an app whose serving instances are
// all dead is reported stopped, but the persisted record is repaired rather
// than blindly trusted the other way either.
func (s *DeployState) Reconcile() (LifecycleDecision, error) {
	d := DecideLifecycle(s)
	changed := false
	fix := func(inst *Instance) {
		if inst == nil {
			return
		}
		alive := InstanceAlive(inst)
		switch {
		case !alive && inst.PID > 0:
			inst.PID = 0
			inst.Status = "stopped"
			changed = true
		case alive && inst.Status != "running":
			inst.Status = "running"
			changed = true
		}
	}
	if s.Mode == ModeBlueGreen {
		for _, inst := range s.Slots {
			fix(inst)
		}
	} else {
		for _, inst := range s.Replicas {
			fix(inst)
		}
	}
	if !changed {
		return d, nil
	}
	if err := Store(s); err != nil {
		return d, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: persist reconciled state for %s", s.AppName)
	}
	return d, nil
}

// ReapStaleInstances clears instance records whose recorded process is
// definitely gone, without touching the state file (the caller persists). It
// is the deploy-flow counterpart of Reconcile: a deploy or rollback that
// loads state must not inherit dead PIDs from an aborted predecessor.
//
// Conservative by design: a live PID whose binary cannot be verified is left
// alone here — it may still be serving traffic (e.g. the recorded binary was
// replaced on disk); read-side Reconcile and InstanceAlive-based derivation
// handle that ambiguity without dropping a possibly-live record mid-deploy.
func ReapStaleInstances(state *DeployState) {
	if state == nil {
		return
	}
	reap := func(instances map[string]*Instance) {
		for _, inst := range instances {
			if inst == nil {
				continue
			}
			// Docker: liveness is the label-verified container check (a PID<=0
			// docker instance was skipped before, so a dead container never got
			// reaped). A gone/removed managed container clears the record.
			if inst.ContainerID != "" {
				if !dockerContainerAlive(inst.ContainerID) {
					inst.PID = 0
					inst.Status = "stopped"
				}
				continue
			}
			// Native: unchanged — conservative bare-liveness check (a live PID
			// whose binary can't be verified is deliberately left alone here).
			if inst.PID <= 0 {
				continue
			}
			if !pidAlive(inst.PID) {
				inst.PID = 0
				inst.Status = "stopped"
			}
		}
	}
	reap(state.Slots)
	reap(state.Replicas)
}

// instanceRef renders a human label for logs that works for both runtimes: the
// short container id for a docker instance, the host pid for a native one.
func instanceRef(inst *Instance) string {
	if inst.ContainerID != "" {
		id := inst.ContainerID
		if len(id) > 12 {
			id = id[:12]
		}
		return "container " + id
	}
	return "pid " + strconv.Itoa(inst.PID)
}

// TeardownDeployment stops a proxy-managed deployment: every recorded
// instance (verified by executable path, never by bare PID) is gracefully
// stopped and the persisted records are marked stopped. Historical metadata
// (active slot, versions, health tier) is preserved so a later
// StartDeployment can restore the deployment as it was.
//
// Removing the proxy route is the caller's job (it needs the CLI's proxy
// client); TeardownDeployment only owns processes and deploy.json.
func TeardownDeployment(ctx context.Context, state *DeployState, grace time.Duration, log Logger) error {
	if state == nil || state.AppName == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "deploy: teardown requires app state")
	}
	if log == nil {
		log = &nopLogger{}
	}
	if grace <= 0 {
		grace = DefaultGracePeriod
	}

	stopAll := func(instances map[string]*Instance) {
		for _, inst := range instances {
			// A docker instance records PID 0 — its identity is the ContainerID,
			// so skipping on PID<=0 left the container running while teardown
			// lied "stopped". Present = live PID OR a ContainerID.
			if inst == nil || (inst.PID <= 0 && inst.ContainerID == "") {
				continue
			}
			if !InstanceAlive(inst) {
				// Stale record (dead/recycled PID, or a gone container): clear it.
				inst.Status = "stopped"
				inst.PID = 0
				continue
			}
			log.Stepf("stopping %s instance %s (grace %s)", state.AppName, instanceRef(inst), grace)
			// stopInstance routes ContainerID -> docker stop/kill and PID ->
			// verified host-signal stop, and (via GracefulStop) escalates to
			// SIGKILL / `docker kill` after grace for an app that ignores SIGTERM.
			report := stopInstance(ctx, inst, grace, 0)
			if report.ForceKilled {
				log.Warnf("instance %s ignored SIGTERM; SIGKILL applied", instanceRef(inst))
			}
			inst.Status = "stopped"
			inst.PID = 0
		}
	}
	stopAll(state.Slots)
	stopAll(state.Replicas)

	if err := Store(state); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: persist teardown state for %s", state.AppName)
	}
	return nil
}

// StartOptions configures StartDeployment.
type StartOptions struct {
	// Launcher starts an instance; current app resource policy is used when nil.
	Launcher InstanceLauncher
	// ListenTimeout bounds the wait for a restored instance to bind its
	// internal port. Defaults to 10s.
	ListenTimeout time.Duration
	// Logger receives progress lines; nil discards them.
	Logger Logger
}

// StartDeployment restores a stopped blue-green/rolling deployment from its
// persisted state: each instance that should be serving (the active slot, or
// every recorded replica) is relaunched from its recorded binary on a fresh
// internal port. The caller is responsible for re-establishing the proxy
// route (it needs the proxy client) and for reconciling the application
// lifecycle record.
func StartDeployment(ctx context.Context, state *DeployState, opts StartOptions) error {
	if state == nil || state.AppName == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "deploy: start requires app state")
	}
	if opts.ListenTimeout <= 0 {
		opts.ListenTimeout = 10 * time.Second
	}
	log := opts.Logger
	if log == nil {
		log = &nopLogger{}
	}

	var toStart []*Instance
	switch state.Mode {
	case ModeBlueGreen:
		inst := state.ActiveInstance()
		if inst == nil || inst.BinaryPath == "" {
			return phelixerr.Newf(phelixerr.CodeNotFound,
				"no deployable instance recorded for %s (active slot %q); run 'phelix rebuild %s --blue-green' first",
				state.AppName, state.ActiveSlot, state.AppName)
		}
		toStart = []*Instance{inst}
	case ModeRolling:
		if len(state.Replicas) == 0 {
			return phelixerr.Newf(phelixerr.CodeNotFound,
				"no replicas recorded for %s; run 'phelix rebuild %s --replicas N' first",
				state.AppName, state.AppName)
		}
		for _, k := range sortedReplicaKeys(state.Replicas) {
			if inst := state.Replicas[k]; inst != nil && inst.BinaryPath != "" {
				toStart = append(toStart, inst)
			}
		}
		if len(toStart) == 0 {
			return phelixerr.Newf(phelixerr.CodeNotFound, "no deployable replicas recorded for %s", state.AppName)
		}
	default:
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "unsupported deploy mode %q", state.Mode)
	}

	// Resolve the runtime to restore with. Prefer the persisted state.Runtime;
	// fall back to docker when an older state predates the Runtime field but the
	// instance to restore carries a ContainerID (so a pre-field docker deploy is
	// not regressed into the native binary launcher).
	runtime := state.Runtime
	if runtime == "" && toStart[0].ContainerID != "" {
		runtime = RuntimeDocker
	}
	if opts.Launcher == nil {
		opts.Launcher = LauncherForRuntime(runtime, state.Network, state.AppName)
	}

	// The on-disk existence check is native-only: for docker, BinaryPath is an
	// image reference (e.g. "app:v11"), not a file, so stat always fails. The
	// docker launcher errors clearly on a missing image; native still errors
	// NOT_FOUND when the recorded binary is truly gone (behavior unchanged).
	if !IsDockerRuntime(runtime) {
		if _, err := os.Stat(toStart[0].BinaryPath); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeNotFound, err, "deploy: recorded binary for %s is gone", state.AppName)
		}
	}

	for _, inst := range toStart {
		envOverlay, err := EnvOverlayFromSnapshot(inst.EnvPath, state.AppID)
		if err != nil {
			return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "deploy: load env snapshot for %s", state.AppName)
		}
		log.Stepf("starting %s instance on slot %s", state.AppName, inst.Slot)
		proc, newPort, err := opts.Launcher(ctx, inst.BinaryPath, envOverlay)
		if err != nil {
			return phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "deploy: start %s instance", state.AppName)
		}
		addr := hostPort(newPort)
		if !port.WaitForListener(addr, opts.ListenTimeout) {
			_, _ = GracefulStop(ctx, proc, 3*time.Second, 0)
			return phelixerr.Newf(phelixerr.CodePortUnavailable,
				"restored %s instance did not listen on %s within %s; see ~/.phelix/logs",
				state.AppName, addr, opts.ListenTimeout)
		}
		inst.PID = proc.PID()
		// Record the fresh runtime identity. For docker this is the NEW
		// container's id; without it InstanceAlive would keep checking the old
		// (now-stopped) container and report the restored app as stopped. For a
		// native process containerIDOf returns "" (unchanged).
		inst.ContainerID = containerIDOf(proc)
		inst.Port = newPort
		inst.Status = "running"
		inst.StartedAt = time.Now()
		log.Successf("instance %s listening on %s", instanceRef(inst), addr)
	}

	if err := Store(state); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: persist start state for %s", state.AppName)
	}
	return nil
}

// RestoreProxyRoute re-establishes the proxy route for an app whose instances
// were just restored by StartDeployment. Apps already enrolled get a Switch;
// unenrolled apps get an Add (first restore after a proxy wipe).
func RestoreProxyRoute(ctx context.Context, state *DeployState, pc ProxyClient, log Logger) error {
	if pc == nil {
		return phelixerr.New(phelixerr.CodeProxy, "no proxy client")
	}
	if log == nil {
		log = &nopLogger{}
	}
	var primary proxy.Target
	var backends []proxy.Target
	switch state.Mode {
	case ModeBlueGreen:
		inst := state.ActiveInstance()
		// Present = live PID (native) OR a ContainerID (docker records PID 0),
		// so a restored docker route is not skipped as "no running instance".
		if inst == nil || (inst.PID <= 0 && inst.ContainerID == "") {
			return phelixerr.Newf(phelixerr.CodeNotFound, "no running instance for %s", state.AppName)
		}
		primary = proxy.Target{Host: hostPort(inst.Port), Label: inst.Slot}
	case ModeRolling:
		for _, k := range sortedReplicaKeys(state.Replicas) {
			inst := state.Replicas[k]
			if inst == nil || (inst.PID <= 0 && inst.ContainerID == "") {
				continue
			}
			t := proxy.Target{Host: hostPort(inst.Port), Label: "replica-" + k}
			if primary.Host == "" {
				primary = t
				continue
			}
			backends = append(backends, t)
		}
		if primary.Host == "" {
			return phelixerr.Newf(phelixerr.CodeNotFound, "no running replicas for %s", state.AppName)
		}
	default:
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "unsupported deploy mode %q", state.Mode)
	}

	enrolled := false
	if st, err := pc.Status(ctx, state.AppName); err == nil && len(st) > 0 {
		enrolled = true
	}
	if enrolled {
		if err := pc.Switch(ctx, state.AppName, primary, backends...); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeProxy, err, "proxy switch failed for %s", state.AppName)
		}
		log.Successf("proxy route restored: :%d -> %s", state.PublicPort, primary.Label)
		return nil
	}
	if err := pc.Add(ctx, state.AppName, state.PublicPort, primary, backends...); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeProxy, err, "proxy enrol failed for %s", state.AppName)
	}
	log.Successf("proxy route restored: :%d -> %s", state.PublicPort, primary.Label)
	return nil
}

// waitForPortRelease polls until the port is bindable again (the previous
// owner exited and the kernel released the socket), or the timeout elapses.
func waitForPortRelease(publicPort int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if port.IsAvailable(publicPort) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// handOffPublicPort frees the public port before the first proxy enrolment.
// A classic instance (started before the app migrated to blue-green) binds
// the public port directly; the proxy cannot take ownership while that
// process lives. The handoff callback (wired by the CLI layer) stops the
// classic instance — after the replacement is already healthy, so the
// downtime window is only the seconds between the old process exiting and
// the proxy binding the port.
func (bg *BlueGreen) handOffPublicPort(ctx context.Context, proc Process, log Logger) error {
	if bg.PublicPort <= 0 || bg.PortHandoff == nil {
		return nil
	}
	if port.IsAvailable(bg.PublicPort) {
		return nil // nobody owns it — nothing to hand off
	}
	log.Stepf("public port %d is owned by a classic instance; handing it to the proxy", bg.PublicPort)
	if err := bg.PortHandoff(ctx, bg.AppName, bg.PublicPort); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeProxy, err, "classic-to-blue-green handoff failed")
	}
	if !waitForPortRelease(bg.PublicPort, 5*time.Second) {
		return phelixerr.Newf(phelixerr.CodePortUnavailable,
			"public port %d still bound after stopping the classic instance", bg.PublicPort)
	}
	return nil
}
