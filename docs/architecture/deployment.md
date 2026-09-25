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

## Autoscaling decision engine (current status)

> **Architecture-only, not yet user-facing.** The Phase 1 audit flagged
> autoscaling as shipped code with no user documentation. Inspection of the source
> confirms it is a **decision engine only** — there is no CLI surface and no
> executor, so no user guide is provided. This section documents its current state
> for engineers.

`internal/deploy/autoscale.go` states its own scope in a header comment:

> Phase 1 autoscaling: the decision engine only observes metrics and decides
> whether the desired replica count of a rolling deployment should change. It
> starts/stops nothing, touches no proxy state, no `deploy.json` and no rebuild
> pipeline; Phase 2 wires a metrics collector (per-replica CPU, proxy p95) and a
> decision executor on top.

- `Autoscaler` (`NewAutoscaler`) is a pure metrics-in / decision-out function:
  `Evaluate(AutoscaleMetrics{CPUPercent, P95Latency}, currentReplicas)` returns a
  `ScaleDecision` (`ScaleNone` / `ScaleUp` / `ScaleDown`, plus current/desired
  replica counts and a human reason). `NewAutoscaler`/`Evaluate` are **not
  constructed anywhere outside tests** — nothing applies the decision yet.
- Rules: scale up when `cpu >= scale_up` **OR** `p95 >= scale_up_p95`; scale down
  when `cpu <= scale_down` **AND** `p95 <= scale_down_p95`. A condition must hold
  for `EvaluationWindows` consecutive samples before a decision; every decision
  moves the desired count by exactly one replica; no further decision until
  `Cooldown` elapses (cooldown clears partial windows to break up→down→up
  oscillation). Zero metrics count as "idle": they satisfy scale-down, never
  scale-up. `MaxReplicas`/`MinReplicas` clamp the decision.
- Config is the `deploy.autoscaling` block in `phelix.yaml`
  (`internal/project/schema.go`: `AutoscalingConfig` → resolved to
  `AutoscaleSettings` via `Resolve(replicas)`). Phase 1 only **loads, validates and
  resolves** it. An unset `max_replicas` defaults to `deploy.replicas` so enabling
  autoscaling alone never widens the replica set beyond what the user declared.
  Defaults: interval 15s, cooldown 60s, CPU up/down 70%/30%, p95 up/down
  500ms/150ms, evaluation windows 1.

Because nothing executes these decisions, `deploy.autoscaling` is intentionally
**not** promoted as a user-facing feature. When an executor and metrics collector
are wired up, a user guide (`guides/autoscaling.md`) and full config documentation
become appropriate.

## Related

- [State management](state-management.md) — `deploy.json`, `versions.json`,
  two-phase promotion, the deploy lock.
- [Zero-downtime deployments](../guides/zero-downtime-deployments.md),
  [Canary & progressive rollouts](../guides/canary-and-progressive-rollouts.md).
- [Resource limits](../guides/resource-limits.md) — the per-instance cgroups the
  autoscaler's CPU signal would observe.
