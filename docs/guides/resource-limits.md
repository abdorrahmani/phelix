# Per-Instance Resource Limits (Linux)

Phelix can apply per-instance CPU and memory limits on Linux via cgroups v2.
Configure them in `phelix.yaml`:

```yaml
resources:
  cpu: "500m"
  memory: "512Mi"
```

Both fields are optional. CPU accepts millicores (`250m`, `500m`, `1000m`) or
cores (`0.5`, `1`, `2`), with at most three decimal places. The fixed `cpu.max`
period is 100000 microseconds: `500m` writes `50000 100000`. The kernel's
minimum quota requires at least `10m`; smaller values fail validation rather than
being rounded up. Memory accepts positive integer quantities with binary units
`Ki`, `Mi`, `Gi`, or `Ti`; `512Mi` writes `536870912` to `memory.max`. Zero,
negative, malformed, overflowing, empty, and unsupported-unit quantities are
rejected. CPU is a bandwidth ceiling, not CPU affinity or a reserved core.

Each running instance gets its own cgroup, including each replica and both
old/new instances during a deployment. Descendants inherit that instance's
limits. An omitted field adds no limit for that resource; omitting the section or
using `resources: {}` adds no Phelix limits for a new app. Host/ancestor cgroup
limits still apply.

## Persistence and rollback

Build/rebuild saves this policy as current app runtime configuration in
`apps.json`, separate from versioned artifacts. Subsequent starts, restarts, and
rollbacks use that current policy; **rollback does not restore historical
resource settings**. A rebuild with an existing YAML file but no `resources`
section clears the saved policy; a missing YAML file leaves it unchanged. Changes
take effect on newly launched instances, not live processes.

## Host requirements

Linux cgroups v2, Linux 5.7+ with `clone3` permitted, and a writable delegated
parent with the requested `cpu`/`memory` controllers already enabled in
`cgroup.subtree_control`. Set `PHELIX_CGROUP_ROOT` to its clean absolute path, or
Phelix uses its current cgroup resolved through `/proc/self/mountinfo` and
`/proc/self/cgroup`. The parent generally must be empty to enable domain
controllers; the launcher should run in a sibling or child cgroup, with the
required delegation permissions. Phelix does not move the launcher or alter
ancestor controller settings. Containers and systemd services must supply
suitable delegation and syscall permissions.

Phelix configures limits before atomically spawning the process into the cgroup
(`CLONE_INTO_CGROUP`), so no unrestricted launch window exists. Setup or
attachment failure aborts launch; it never retries without limits. Non-Linux
hosts reject configured limits. Apps without limits retain the existing process
launch path.

## Cleanup

A detached cleanup helper waits for the instance cgroup to become empty, then
removes only that owned leaf. Cleanup covers normal exit, crash, and failed
startup, remains idempotent, and survives the CLI exiting. Descendants keep the
cgroup alive until they exit. The helper must not be killed by an external
supervisor; no reboot recovery or orphan scavenger is provided. This applies to
native app processes, not Docker/Kubernetes workloads or build commands, and adds
no resource monitoring dashboard.

## Memory-limit violations (RESOURCE_OOM)

Memory-limit violations are detected through the instance cgroup's cgroup-v2
`memory.events` counters: an instance is classified as **resource OOM** when
`oom_kill` increases during its lifetime, not from the exit status or `SIGKILL`
alone (a plain `SIGKILL` without a cgroup OOM kill keeps its existing meaning).
The classification unit is the cgroup, so a killed descendant counts even if the
main process survives. The launcher captures the counter baseline before launch
and reads the final counters before the cleanup lease is released, so cleanup
never erases the evidence. Resource OOM surfaces as the structured `RESOURCE_OOM`
error — distinct from a generic process crash, a non-zero exit, and a failed
health check — and carries the configured limit and PID in its message. Exits for
any other reason (including unreadable evidence) keep the existing behavior.

## Deployment safety

Deployment safety is unchanged in shape: a blue-green candidate that hits its
memory limit fails its health check and never becomes the serving instance — the
deployment fails with the resource-OOM reason while the old instance keeps
serving. A rolling replacement killed by its memory limit is rejected through the
existing rolling failure/recovery path (previous replica restored and still
serving) with the resource-OOM reason attached. Rollback semantics are untouched:
a rollback still uses the current saved resource policy, never the target
version's historical settings.

## Current limitations

Current memory usage (`memory.current`/`memory.max`), memory event counters, and
CPU accounting (`cpu.stat`) are readable internally by the runtime layer for
lifecycle classification and future monitoring, but no dashboard, backend API, or
live resizing exists in this phase; resource configuration changes still apply
only to newly launched instances.

> Replica **autoscaling** based on these metrics exists as a decision engine
> only and is not yet wired to execute — see
> [deployment architecture](../architecture/deployment.md#autoscaling-decision-engine-current-status).

## Related

- [Configuration](../reference/configuration.md) — the `resources` block.
- [Environment reference](../reference/environment.md) — `PHELIX_CGROUP_ROOT`.
- [Error codes](../reference/error-codes.md) — `RESOURCE_OOM`.
- [Deployment architecture](../architecture/deployment.md).
