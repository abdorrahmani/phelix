# Deployment Engine

This document describes the internals of Phelix's zero-downtime deployment
engine. For the user-facing workflow see
[Zero-downtime deployments](../guides/zero-downtime-deployments.md) and
[Canary & progressive rollouts](../guides/canary-and-progressive-rollouts.md).

## Core invariants

- **Fail-safe deploys.** Blue-green/rolling never point `current` or proxy traffic
  at an instance that hasn't passed health. A failed candidate is killed; the
  active instance is untouched. Rolling never takes more than one replica out of
  rotation.
- **Atomic proxy cut-over.** `internal/proxy` binds each app's public port once for
  the daemon's lifetime and reads the target from an `atomic.Value` in the
  per-request `Director` — switching takes effect on the next request, no locking,
  no dropped connections. `~/.phelix/proxy.sock` exposes
  `Add`/`Switch`/`Status`/`Shutdown`; the CLI and deploy flow both talk to it via
  `proxy.Client`.
- **Process identity is verified before signalling.** `findVerifiedProcess(pid,
  binaryPath)` gates every kill, so a recycled PID can never be mistaken for a live
  instance or get an unrelated process killed. (In the Docker runtime the
  equivalent check is container id + `phelix.managed` label.)
- **Deploy mutual exclusion is an OS file lock** (`flock`/`LockFileEx` on
  `deploy.lock`), not a check-then-write on `deploy.json`. The kernel releases it
  on death, so stale locks are impossible by construction. `phelix deploy unlock`
  exists for the rare manual case.

## Blue-green sequence

`internal/deploy/bluegreen.go`:

1. Load/init `deploy.json`; pick the inactive slot.
2. Ensure the proxy daemon is reachable.
3. Build the new binary (or resolve the rollback target).
4. Launch the instance on the inactive slot's internal port.
5. Select the health tier; warn (terminal + panel) if Tier 2/3.
6. `WaitForHealthy`; on failure kill the new instance, leave active untouched.
7. Atomically `ProxyClient.Switch` (or `Add` on first deploy).
8. Promote version + persist state; gracefully stop the old slot (grace period,
   force-kill after timeout, report in-flight count).

Rolling (`rolling.go`) does the same but restarts replicas one at a time, never
taking more than one out of rotation; a health failure aborts and leaves N-1
serving.

`cmd/rebuild.go` wires everything together through a `deploy.FreshBuildSource`
(`internal/deploy/source.go`) that delegates to the existing `rebuildApp` path,
plus `deploy.DefaultLauncher`, `deploy.DefaultHealthProvider`, and a colored logger
(`colorLogger`) that matches the project's `→ ✓ ⚠ ✗` convention.

## Health-tier resolution

`internal/deploy/health.go:selectDeployHealth` is the single tier-resolution point
both deploy strategies share — it is not duplicated per strategy. Tier semantics
are described in [Health checks](../guides/health-checks.md) and
[monitoring architecture](monitoring.md).

## Lifecycle state is derived, not stored

Lifecycle state shown by `list`/`status` is **derived from deployment reality**
(`internal/deploy/lifecycle.go`: `InstanceAlive`, `ServingInstance`), not read from
a possibly-stale `apps.json` record. `deploy.json` exists so zero-downtime deploys
never have to distort the single-PID `apps.json` model — see
[state management](state-management.md).

## Three distinct port concepts

Public proxy port (`:8080`, owned by the proxy) ≠ internal instance port (assigned
per slot/replica) ≠ the `PORT` env var Phelix injects to tell each instance which
internal port to bind. Managed apps must read `PORT`; `phelix doctor` catches
hardcoded ports. See [The PORT Contract](../getting-started/the-port-contract.md).

## Autoscaling architecture

The autoscaling engine is now fully wired: the monitor daemon runs a per-app
controller (`internal/autoscale`) that samples metrics, evaluates decisions and
executes them through the existing scaling paths. The decision engine itself
remains a pure metrics-in / decision-out function in
`internal/deploy/autoscale.go`.

- **Sampler** (`internal/autoscale/sampler.go`): per-replica CPU (interval
  deltas over gopsutil process times), per-replica memory (RSS over host
  memory), and windowed proxy traffic (RPS, error rate, P95, P99) by diffing
  the cumulative `BackendStat` counters with negative clamping after proxy
  restarts. Missing replicas flag their dimension missing; the first traffic
  window only seeds the baseline.
- **Decision engine**: scale up when ANY enabled metric breaches its upper
  threshold, scale down when EVERY enabled metric is at or below its lower
  threshold; up wins conflicts. Per-direction evaluation windows and cooldowns,
  a scale-down stabilization window, freshness rejection (a sample older than
  twice the interval decides nothing), and `ConfirmExecution`/`AbortDecision`
  so a failed execution never consumes its cooldown. Every decision moves the
  desired count by exactly one replica; min/max clamp.
- **Execution**: worker decisions dispatch to `deploy.WorkerScale` (which takes
  the app deploy lock itself); rolling decisions go through the rolling scale
  executor (`internal/deploy/scale_exec.go`), which takes the same lock, loads
  state, rejects stale decisions and rollouts in flight, launches the active
  version's artifact (no rebuild, no new version), health-gates the new
  replica, updates proxy membership, commits `deploy.json`, and drains victims
  membership-first. A `ScaleProgress` marker plus `RecoverInterruptedScale`
  converge an interrupted scale on daemon restart.
- **Controller** (`internal/autoscale/controller.go`): per-app single-flight
  evaluation loop at the configured interval, pause state (`autoscale.json`,
  survives restarts), append-only decision history
  (`autoscale_history.jsonl`), and an `ops` record per execution.
- **Surfaces**: `phelix autoscale status|enable|disable|pause|resume|evaluate|history`
  locally, and `autoscale_*` MonitorStream commands remotely, gated by the
  `scale` authorization action.

Config is the `deploy.autoscaling` block in `phelix.yaml`
(`internal/project/schema.go`: `AutoscalingConfig` → resolved to
`AutoscaleSettings` via `Resolve(replicas)`). An unset `max_replicas` defaults
to `deploy.replicas` so enabling autoscaling alone never widens the replica set
beyond what the user declared. Defaults: interval 15s, cooldown 60s (both
directions unless overridden), CPU up/down 70%/30%, p95 up/down 500ms/150ms,
evaluation windows 1, stabilization off. See the
[configuration reference](../reference/configuration.md#autoscaling-block).

## Related

- [State management](state-management.md) — `deploy.json`, `versions.json`,
  two-phase promotion, the deploy lock.
- [Zero-downtime deployments](../guides/zero-downtime-deployments.md),
  [Canary & progressive rollouts](../guides/canary-and-progressive-rollouts.md).
- [Resource limits](../guides/resource-limits.md) — the per-instance cgroups the
  autoscaler's CPU signal would observe.
