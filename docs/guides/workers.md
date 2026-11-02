# Workers: Supervised Background Workloads

A worker is a long-running background process with no public port: a Redis or
Kafka consumer, an email sender, a background processor. Phelix treats it as a
first-class workload type (`type: worker`) alongside services, with real
process supervision, replica sets, rolling replacement and exec health
checks. The service model is unchanged.

```yaml
name: email-worker
type: worker

process:
  restart:
    policy: on-failure        # always | on-failure | never
    max_attempts: 10          # consecutive failure-driven restarts before crashloop
    backoff:
      initial: 2s
      max: 60s
      multiplier: 2
  shutdown:
    signal: SIGTERM           # SIGTERM (default) or SIGINT
    grace_period: 30s         # SIGKILL escalation after this wait
  startup_timeout: 30s

health:
  mode: process               # process (default) or exec
  # exec:
  #   command: ["/bin/sh", "-c", "test ! -f /tmp/worker-unhealthy"]
  #   interval: 10s
  #   timeout: 5s
  #   retries: 3

deploy:
  strategy: rolling           # classic (stop-all, start-all) or rolling
  replicas: 3
  rolling:
    max_surge: 1              # v1 replaces one replica at a time
    max_unavailable: 0
    drain_timeout: 60s        # SIGTERM wait when retiring a replaced replica

resources:                    # per replica, cgroups v2 on Linux
  cpu: 500m
  memory: 512Mi
```

A worker never allocates a port, never receives a `PORT` variable, never gets
proxy routes, and never fails a port gate: startup success means the process
is alive and (in exec mode) passing its probe within `startup_timeout`.
Workers are native-runtime only in v1; a `deploy.runtime: docker` worker is
rejected at validation.

## Process supervision

The monitor daemon (`phelix monitor`) runs one supervisor loop that owns every
worker app. For each replica it reconciles the persisted desired state against
the observed process, keyed by full process identity: PID, executable path and
the kernel start time from `/proc/<pid>/stat`, so a recycled PID is never
mistaken for the original process.

Exit handling follows the restart policy:

| policy | exit 0 | non-zero / signal / OOM |
|---|---|---|
| `always` | restart (counts toward max_attempts) | restart |
| `on-failure` | no restart, replica marked `completed` | restart |
| `never` | no restart, `completed` | no restart, `failed` |

Restart delays grow `initial * multiplier^attempt`, capped at `max`, with
+/-20% jitter so a fleet crashing together does not stampede a shared Redis or
database. Once a replica stays up for the stable window (300s), its
consecutive-restart counter resets. After `max_attempts` consecutive failures
the replica enters `crashloop` and stays there until a manual restart or a
redeploy resets it.

Replica states: `starting`, `running`, `unhealthy`, `restarting`, `stopping`,
`draining`, `stopped`, `completed`, `failed`, `crashloop`. The deploy engine
and CLI commands own `stopping`/`draining`/`replacing`; the supervisor never
touches a replica in those states, and every component takes the same per-app
lifecycle lock (`ReplicaCoordinator`) before deciding anything, so a deploy
and the supervisor can never race over one replica.

## Health checks

`process` mode (default) watches identity liveness. `exec` mode runs the
configured argv directly, without a shell, from the app directory with the
replica's environment, enforcing `timeout` with a process-group kill so a
spawning probe can never leak children; the last 4 KiB of probe output
surface in failure reasons. `retries` consecutive failed probes cause ONE
health-driven restart (one restart attempt consumed, regardless of how many
probes failed), counted toward `max_attempts`. Probes for one replica never
overlap.

## Replicas and day-2 operations

```sh
phelix status email-worker             # replica tree: pid, status, restarts, last exit
phelix scale email-worker --replicas 5 # grow/shrink in place, no rebuild
phelix restart email-worker --replica 2  # one replica; resets its crash counters
phelix restart email-worker            # every replica
phelix stop email-worker               # graceful stop of every replica
phelix log email-worker --replica 2    # one replica's output
phelix list                            # deploy column shows worker (xN)
```

Scale-up launches new slots at the current version from the recorded env
snapshot; scale-down drains the highest-index replicas as intentional stops
(they never count as crashes). The backend can scale remotely through the
`worker_scale` MonitorStream command (idempotent by request key, like the
webhook family).

## Deployments and recovery

`phelix rebuild` on a worker runs the worker engine: `rolling` replaces one
replica at a time (launch replacement, prove it via the startup gate, then
drain the old replica within `drain_timeout`); `classic` drains everything
first. There is no proxy step anywhere. Every per-replica transition is
journaled in the deploy state, and on failure the engine reverts exactly the
replicas it already replaced. If a revert step itself fails, the state is
persisted as `recovery_required` and surfaced by `status` and the daemon log;
Phelix never reports a rollback that did not happen. An interrupted rollout
(daemon died mid-deploy) is detected by the journal and reverted at the next
daemon start or deploy.

v1 draining is a SIGTERM plus a bounded wait: Phelix sends the signal and
holds the window, but whether in-flight work is truly finished is the app's
contract. Treat SIGTERM as "finish the current message, ack or nack it, then
exit"; a parallel- surge/unavailable policy and an explicit drain protocol
are planned extensions.

## After a daemon restart

The monitor adopts replicas whose recorded identity still matches a live
process, marks dead ones with their exit bookkeeping, and restarts them per
policy with counters preserved. Crashloop state survives daemon restarts.
Processes running a worker binary outside the deploy state are reported as
orphans at daemon start and by `status` (detection only; Phelix never kills a
process it cannot attribute).

## Example

`examples/worker-go` is a runnable demo: `phelix build email-worker`, then
watch `phelix status` while you `kill -9` a replica, `touch
/tmp/worker-go-unhealthy` (with the exec probe configured) or change
`deploy.replicas` and rebuild.
