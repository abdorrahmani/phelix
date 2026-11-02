# Worker Remote Contract

Status: implemented on `feat-worker`.

Scope: what a `type: worker` app reports over the agent wire protocol and what
the backend may send to control it. Everything here reuses existing messages
and the existing deploy engine. No second worker implementation, no new RPC.

A worker is a port-less supervised background process. It has no proxy route,
no listener and no public port. It runs one or more native replicas on the host;
each replica is judged by process identity plus the configured startup gate.

## 1. Capability gating

The agent advertises `worker_lifecycle` in `CLIMetadata.capabilities`. Both
worker-specific commands (`worker_scale`, `worker_restart_replica`) are gated on
it. An absent capability means the backend must not offer worker controls.

The generic lifecycle commands (`start`, `stop`, `restart`, `remove`,
`rebuild`) need no extra capability. They already handle worker apps because a
worker is deploy-managed: the daemon routes them through the same CLI paths a
local invocation uses.

## 2. How a worker appears

A worker is a normal managed app. It reaches the dashboard through the existing
`ApplicationInfo` message (`MonitorEvent.app_info`), sent on every monitoring
tick (2 second cadence while the app is watched).

Additive field:

| Field | Type | Meaning |
| --- | --- | --- |
| `workload_type` | string | `worker` for a `type: worker` project, `service` otherwise. Older agents send empty, which means `service`. |

Worker semantics of the existing fields:

| Field | Meaning for a worker |
| --- | --- |
| `port` | Always `0`. A worker never listens. |
| `status` | Derived from live replica state, not from the persisted app record: `running` when every desired replica is alive, `degraded` when at least one is alive but not all, `stopped` when none are alive. A `crashloop` or `failed` replica keeps the app out of `running`. |
| `pid` | PID of the lowest-slot alive replica, `0` when none is alive. |
| `uptime` | Elapsed time since that replica started. `N/A` when no replica is alive. |
| `language`, `build_status`, `created_at`, `updated_at` | Unchanged meaning. |
| `process`, `networking`, `logging`, `storage` | Legacy app configuration groups. They are not the worker supervision policy; read `DeploymentSnapshot.worker` for that. |

The status and PID are read from the app's live deploy state on every tick, so
they follow supervisor restarts without waiting for a CLI command to rewrite
the app record.

## 3. Resource metrics

`AppResourceMetrics` carries one series per app:

| Field | Meaning for a worker |
| --- | --- |
| `cpu_usage` | Sum of CPU percent across every alive replica. |
| `memory_usage` | Sum of resident memory bytes across every alive replica. |

No metric is sent for a tick where no replica is alive. The values aggregate
the replica set; per-replica process detail is in the deployment snapshot.

## 4. Worker replica logs

Only a worker's replica files are read. A worker writes one log file per
replica instead of the combined app file a service uses, and each line carries
the replica slot that produced it.

Additive field on `MonitorLogEntry`:

| Field | Type | Meaning |
| --- | --- | --- |
| `replica` | string | Worker replica slot (`"0"`, `"1"`, ...). Empty for service apps and self logs. |

Unchanged fields on the same entry remain authoritative: `app_id` is the app's
stable ID, `app` is its name, `version` is the version that replica runs, and
`source` is `LOG_SOURCE_APPLICATION` for all app output. Lines are redacted and
batched by the existing log path.

Log rotation covers every replica file, so a long-running noisy replica cannot
grow without bound.

## 5. Deployment snapshot

The worker topology is carried by the existing `DeploymentSnapshot`, on
reconnect and on the 60 second resync, and inside deployment events.

Discriminator: `strategy` is `"worker"`.

### 5.1 Replica fields

Every entry in `repeated DeploymentReplica` describes one replica. Slots are
stable: `index` is the numeric slot, `id` is `replica-<index>`.

Existing fields keep their meaning (`status`, `health`, `pid`, `started_at`,
`healthy_at`, `version`, `image`, `container_id`). Worker statuses include
`running`, `starting`, `restarting`, `replacing`, `stopping`, `crashloop`,
`failed` and `stopped`.

Additive fields:

| Field | Type | Meaning |
| --- | --- | --- |
| `restart_count` | int32 | Consecutive failure-driven restarts. Reset by a stable window or a manual restart. |
| `last_restart_at` | int64 | Unix millis of the last restart decision. `0` means never recorded. |
| `last_exit_code` | int32 | Exit status of the previous run. `0` clean, `1..255` failure, `-1` unknown (adopted process with no waitable handle). |
| `last_exit_reason` | string | Human reason recorded with the exit, for example a crashloop rest stop or a failed probe. |
| `last_exit_at` | int64 | Unix millis of the last observed exit. `0` means never recorded. |
| `ready_at` | int64 | Unix millis the replica last passed its startup gate. `0` means never recorded. |

`0` in these fields means the replica never recorded the event. It never means
"healthy" or "no restarts". Use `restart_count` for the count, not the
timestamps.

### 5.2 Supervision configuration

Additive `DeploymentWorkerConfig` message on `DeploymentSnapshot.worker`
(field 25). It is absent for every non-worker app.

| Field | Type | Meaning |
| --- | --- | --- |
| `restart_policy` | string | `always`, `on-failure` or `never`. |
| `max_restart_attempts` | int32 | Failure restarts before the replica enters `crashloop`. |
| `backoff_initial` | string | Initial restart delay, Go duration string. |
| `backoff_max` | string | Maximum restart delay, Go duration string. |
| `backoff_multiplier` | double | Exponential growth factor. |
| `shutdown_signal` | string | `SIGTERM` or `SIGINT`. |
| `grace_period` | string | Graceful shutdown window, Go duration string. |
| `startup_timeout` | string | Startup gate deadline, Go duration string. |
| `health_mode` | string | `process` (identity only) or `exec` (probe command). |
| `health_command` | repeated string | Exec probe argv. Empty in `process` mode. |
| `health_interval` | string | Probe interval, Go duration string. |
| `health_timeout` | string | Probe timeout, Go duration string. |
| `health_retries` | int32 | Consecutive probe failures before the replica is unhealthy. |
| `rolling_max_surge` | int32 | Rolling replacement surge, currently always `1`. |
| `rolling_max_unavailable` | int32 | Rolling replacement unavailability, currently always `0`. |
| `rolling_drain_timeout` | string | Drain bound, Go duration string. Empty means "use `grace_period`". |

This block describes the policy the replica set runs under. Observed state is
on the replica entries and in the counters below.

### 5.3 Counters

| Field | Meaning for a worker |
| --- | --- |
| `replicas_desired` | Number of recorded replica slots. |
| `replicas_current` | Replicas with a recorded PID. |
| `replicas_ready` | Replicas whose process is alive and status is `running`. |
| `replicas_healthy` | Ready replicas whose health is known healthy. |

Counters are observed values. Never derive `ready` from `desired`.

## 6. Commands

Commands arrive on `MonitorStream` as `MonitorControl.command` holding a
`MonitorCommandRequest`. Each result arrives as `MonitorCommandResult` on the
same stream, correlated by `request_id`.

### 6.1 Generic lifecycle commands

`start`, `stop`, `restart`, `remove` and `rebuild` keep their existing request
shape. For a worker app the agent executes the worker-aware CLI path: start
launches any missing replica, stop gracefully stops the whole replica set,
restart replaces every replica, remove tears the app down, and rebuild runs the
worker build plus rollout.

`rebuild` may carry `strategy` and `replicas` exactly as it does for services.
For a worker, `replicas` changes the desired replica count for the rollout.

### 6.2 `worker_scale`

Request:

| Field | Required | Meaning |
| --- | --- | --- |
| `request_id` | yes | Idempotency key. |
| `type` | yes | `worker_scale`. |
| `app_name` | yes | App stable ID or name. |
| `replicas` | yes | Desired replica count, `>= 1`. |
| `replica` | no | Must be empty. |

The agent runs the same scaling function as the local `phelix scale` command,
under the same per-app replica coordinator lock. Growth launches new slots at
the current version; shrink drains the highest slots as intentional stops.

Result on success:

| Field | Meaning |
| --- | --- |
| `status` | `success`. |
| `from_replicas` | Count before the command. |
| `to_replicas` | Count after the command. |

Scaling to the current count is a successful no-op with equal counts.

### 6.3 `worker_restart_replica`

Request:

| Field | Required | Meaning |
| --- | --- | --- |
| `request_id` | yes | Idempotency key. |
| `type` | yes | `worker_restart_replica`. |
| `app_name` | yes | App stable ID or name. |
| `replica` | yes | Slot to restart, `"0"`, `"1"`, ... matching `DeploymentReplica.index`. |
| `replicas` | no | Must be `0`. |

The agent stops that replica with the configured signal and grace period,
resets its crash counters, relaunches it at its recorded version and leaves
every other replica untouched.

Result on success:

| Field | Meaning |
| --- | --- |
| `status` | `success`. |
| `replica` | The slot that was restarted. |

### 6.4 Rejected options

Both worker commands reject any of `strategy`, `reason`,
`verify_duration_ms`, `dry_run`, `target`, `matrix` and `webhook` when set.
`worker_scale` rejects `replica`; `worker_restart_replica` rejects `replicas`.
A malformed request is answered with `INVALID_ARGUMENT` before any state is
touched.

### 6.5 Idempotency

Both commands use the durable worker ledger, keyed by `request_id`:

- Same `request_id` with the same payload replays the stored terminal result.
- Same `request_id` with a different payload returns `ALREADY_EXISTS`.
- A `request_id` still in flight returns `UNAVAILABLE`.
- A command refused before it started releases its key, so a corrected retry
  with the same `request_id` executes.
- The terminal result is recorded before it is sent, and undelivered results
  are replayed after a reconnect.

The ledger is bounded and keeps only a recent window of delivered entries.

## 7. Errors

| `error_code` | Meaning |
| --- | --- |
| `INVALID_ARGUMENT` | Malformed request, wrong option for the command, or target app is not a worker. |
| `NOT_FOUND` | App or replica slot does not exist. |
| `CONFIGURATION_ERROR` | The app's deploy state is not a worker state, or the worker has no deployed version to scale. |
| `PROCESS_FAILED` | A replica could not be stopped cleanly. |
| `INSTANCE_START_FAILED` | A replica could not be launched. |
| `ALREADY_EXISTS` | `request_id` reused with a different payload. |
| `UNAVAILABLE` | The same command is still in flight, or its result could not be recorded durably. |
| `UNIMPLEMENTED` | No worker handler is registered in the running daemon. |

`error` is a redacted human message. `error_code` is the machine-readable
value. Never parse the message.

## 8. Backend flow

1. Gate worker UI on the `worker_lifecycle` capability.
2. Identify workers by `ApplicationInfo.workload_type == "worker"`.
3. Render status, PID and uptime from `ApplicationInfo`; it refreshes every 2
   seconds and reflects live replicas.
4. Render the replica table from `DeploymentSnapshot.replicas`, using
   `restart_count`, `last_exit_*` and `ready_at` to explain health. Render the
   policy panel from `DeploymentSnapshot.worker`.
5. Attribute log lines to a replica with `MonitorLogEntry.replica`.
6. Send `worker_scale` or `worker_restart_replica` with a fresh `request_id`
   per user action. Reuse the same `request_id` only for a retry of the exact
   same payload.
7. After a successful command, apply the result counters immediately and
   expect the next snapshot resync (at most 60 seconds) or the deployment event
   stream to confirm the new topology.

## 9. Compatibility

All additions are additive protobuf fields on existing messages plus one new
message used only inside `DeploymentSnapshot`. Older backends ignore them.
Older agents send empty `workload_type`, no `replica` on logs, no worker block
on snapshots and do not advertise `worker_lifecycle`, so the backend shows the
pre-worker behavior and offers no worker controls.
