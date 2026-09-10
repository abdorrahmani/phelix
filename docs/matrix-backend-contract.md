# Phelix Remote Build Matrix — Backend Implementation Guide

This document is the **source of truth for the Phase 2 Backend** implementing
remote Build Matrix support. It describes the wire contract the CLI/Agent
(Phase 1) implements today: every command, event, field, enum, state machine,
idempotency rule, and recovery expectation, with examples.

The agent never runs a second matrix implementation: every remote command
drives the same engine as `phelix build --matrix`, `phelix dockerize
--matrix`, `phelix build --matrix --resume`, and `phelix matrix retry`
(profile convergence → plan expansion → executor → run history → report →
version record → release manifest). If this document and the agent code ever
disagree, the code wins — then this document gets fixed.

Architecture:

```
Backend                              CLI/Agent (phelix monitor daemon)
───────                              ─────────────────────────────────
                                     MonitorStream (bidi, persistent)
  ── MonitorCommandRequest ──────►     handleMonitorCommand
        type = matrix_*                  │ validate (request shape)
        matrix = MatrixOptions           │ ledger begin (mutating cmds)
                                        ▼
                                     background goroutine (mutating cmds)
                                        │ RemoteMatrixCommand (cmd pkg)
                                        │   matrix.Resolve / Expand
                                        │   startMatrixSessionContext /
                                        │   runDockerizeMatrixCore
                                        │   completeMatrixSession
                                        ▼
  ◄────── MonitorEvent ──────────     terminal MonitorCommandResult
        payload = command_result        (ledger-persisted before send,
                                          replayed on reconnect)

  ◄────── ReportMatrixEvent (unary)──  MatrixReporter (background sender)
        MatrixEvent                     (best-effort lifecycle events)

  ◄────── MonitorEvent ──────────     MatrixRunState resync snapshot
        payload = matrix_run            (on reconnect + every 60s,
                                          active runs only)
```

---

## 1. Supported remote operations

Six command types, all carried by `MonitorCommandRequest` over the
existing MonitorStream. A matrix command never reaches the lifecycle command
executor (`monitor.CommandExecutor`); it has its own dispatch
(`internal/grpc/matrix_command.go`).

| Command | Mutating | Local twin | Ledger-guarded |
|---|---|---|---|
| `matrix_build` | yes | `phelix build <app> --matrix` | yes |
| `matrix_dockerize` | yes | `phelix dockerize <app> --matrix` | yes |
| `matrix_resume` | yes | `phelix build --matrix --resume[=<id>]` | yes |
| `matrix_retry` | yes | `phelix matrix retry <id> --failed` | yes |
| `matrix_status` | no (query) | `phelix matrix status [id]` | no |
| `matrix_list` | no (query) | `phelix matrix list` | no |

Execution model:

- **Mutating commands execute asynchronously.** The dispatch validates,
  begins the idempotency ledger entry, and returns; the actual matrix
  execution runs on a background goroutine. The MonitorStream receive loop is
  never blocked by a build, and **server/app metrics keep flowing** during
  matrix execution (matrix commands do not pause the metrics tick, unlike
  lifecycle commands).
- **Queries (`matrix_status`, `matrix_list`) execute synchronously** on the
  receive loop: they only read persisted JSON records and locks.
- **Terminal results are durable**: for mutating commands the result is
  persisted to the idempotency ledger *before* the send attempt, and
  re-sent on every MonitorStream (re)connection until delivery is confirmed.
- **Panics are isolated**: a panic in a remote matrix execution converts to
  a terminal `UNKNOWN` error result, never a daemon crash.

What the engine supports (and the wire exposes) — audited from the
implementation, not the README:

- Dimensions: one ecosystem (`go` or `rust`), toolchain versions
  (`major.minor[.patch]`), platforms (`os/arch[/variant]`; known set:
  linux/amd64, linux/arm64, linux/arm/v7, linux/arm/v6, darwin/amd64,
  darwin/arm64, windows/amd64).
- Include/exclude rules — **YAML-only** (`phelix.yaml` matrix profile on the
  agent, in the app's project directory). There is no payload representation
  for rules, exactly as there are no CLI flags for them; whatever rules the
  profile carries apply to the effective dimensions and are reported back in
  the run's configuration snapshot.
- Concurrency (worker-pool bound), automatic retries (additional attempts for
  transient failures only), dry-run, resume, manual retry.
- Per-combination status, attempt history, artifacts, artifact size, SHA-256
  checksums, cache/build classification, durations, redacted errors.
- Release manifest for terminal native runs (succeeded/partial).
- Native matrix (binaries, run-based) vs Docker matrix (images, run-less).

Not supported remotely (matching the CLI): cancelling a running matrix
command (no remote cancellation exists for any command today), interactive
matrix wizard (`phelix matrix init`), editing the phelix.yaml matrix profile
remotely.

---

## 2. Wire contract

### 2.1 Proto changes (Phase 1)

All changes are additive; older backends/agents ignore unknown fields or
oneof cases.

| File | Change | Purpose |
|---|---|---|
| `matrix.proto` (new) | `MatrixOptions`, `MatrixRule`, `MatrixRunConfig`, `MatrixRunConfigSources`, `MatrixAttemptState`, `MatrixCombinationState`, `MatrixCounters`, `MatrixReleaseInfo`, `MatrixRunState`, `MatrixRunSummary`, `MatrixPlanPreview`, `MatrixDockerResult`, `MatrixEvent` | the remote matrix data model |
| `monitoring.proto` | `MonitorCommandRequest.matrix = 10` (`MatrixOptions`) | matrix command options |
| `monitoring.proto` | `MonitorCommandResult.matrix_run_id = 8`, `.matrix_run = 9` (`MatrixRunState`), `.matrix_runs = 10` (`repeated MatrixRunSummary`), `.matrix_preview = 11` (`MatrixPlanPreview`), `.matrix_docker = 12` (`MatrixDockerResult`) | structured terminal results |
| `monitoring.proto` | `MonitorEvent` oneof `matrix_run = 19` (`MatrixRunState`) | reconnect/periodic state resync |
| `phelix.proto` | RPC `ReportMatrixEvent(MatrixEvent) returns (EventResponse)` | asynchronous lifecycle events |

Regenerate with (from the repository root):

```bash
protoc -I. --go_out=. --go_opt=paths=source_relative internal/grpc/proto/*.proto
protoc -I. --go-grpc_out=. --go-grpc_opt=paths=source_relative internal/grpc/proto/*.proto
```

### 2.2 Request: `MonitorCommandRequest` (matrix fields)

Existing fields reused by matrix commands:

| Field | Matrix semantics |
|---|---|
| `request_id = 1` | **Required, non-empty** for every matrix command. Backend-minted correlation ID. Reuse it for retries of the same logical command (see §5). |
| `type = 2` | one of the six `matrix_*` types. |
| `app_name = 3` | Registered application **name or ID**, resolved through the agent's app manager. Required for `matrix_build`/`matrix_dockerize`. Optional guard/filter for the others (see per-command rules). The project directory is the app's registered directory — that is where a local `phelix build` would run and where the app's `phelix.yaml` is read. |
| `target = 6` | Matrix Run selector. `matrix_resume`: `"latest"` or an explicit `mx_…` run ID (required). `matrix_retry`: explicit run ID (required; `"latest"` is rejected). `matrix_status`: `""`/`"active"`/`"latest"`/explicit ID (optional). Must be empty for `matrix_build`, `matrix_dockerize`, `matrix_list`. |
| `dry_run = 9` | Allowed for `matrix_build`, `matrix_dockerize`, `matrix_resume`. Rejected (`INVALID_ARGUMENT`) for `matrix_retry`, `matrix_status`, `matrix_list` — retry has no local dry-run, and queries are already read-only. |

Fields that must NOT be set on matrix commands (validation rejects them):
`strategy`, `replicas`, `reason`, `verify_duration_ms`. They belong to
rebuild/rollback commands; a matrix command carrying them is a backend bug.

New field:

```proto
MatrixOptions matrix = 10;
```

`MatrixOptions` (all optional; empty/zero = "not specified"):

| Field | Type | Semantics |
|---|---|---|
| `go_versions` | `repeated string` | Go toolchain versions. Mutually exclusive with `rust_versions`. |
| `rust_versions` | `repeated string` | Rust toolchain versions. |
| `platforms` | `repeated string` | Target platforms, `os/arch[/variant]`. |
| `concurrency` | `int32` | Worker-pool bound. `0` = engine default (3). Negative rejected. |
| `retries` | `int32` | Automatic-retry budget (additional attempts per failed combination, transient failures only). `0` = no retries. Negative rejected. |
| `build_args` | `repeated string` | Native: appended verbatim to the build tool (like `--build-arg`). Dockerize: must be `KEY=VALUE` (docker `--build-arg`). On resume, non-empty build_args override the run snapshot's recorded args. |
| `tag` | `string` | Native: optional version tag (stored with the recorded version). Dockerize: image tag. |
| `registry` | `string` | Dockerize only: registry prefix for tags/pushes. |
| `push` | `bool` | Dockerize only: push images after building (fail-closed — a partial build is not pushed without `push_partial`). |
| `push_partial` | `bool` | Dockerize only: push even when some combinations failed. |
| `multi_arch_tag` | `bool` | Dockerize only: assemble one multi-arch manifest list per toolchain version from the pushed images. Requires `push`. |
| `list_limit` | `int32` | `matrix_list` only: max summaries. `0` = 20, hard cap 100, negative rejected. |

**Convergence** (`matrix.Resolve`): the payload acts as the "CLI" layer of
the documented precedence — payload > the app's `phelix.yaml` matrix profile
> engine default. Lists **replace, never merge**. An explicit ecosystem that
disagrees with the project's detected language is rejected. Every resolved
dimension's origin is recorded in the run snapshot (`sources`: `cli`,
`phelix.yaml`, `default`, `detected` — payload-provided dimensions are
recorded as `cli`).

### 2.3 Terminal result: `MonitorCommandResult` (matrix fields)

| Field | Set when |
|---|---|
| `matrix_run_id = 8` | Any matrix answer that identifies one run: terminal results of `matrix_build`/`matrix_resume`/`matrix_retry`, and `matrix_status` for a specific/active/latest run. Empty for dockerize, dry-runs, list, errors before a run exists, and the "no active run" status answer. |
| `matrix_run = 9` | Full `MatrixRunState`: the terminal outcome of build/resume/retry (including failures — see §7), and the answer of `matrix_status` for a specific run. |
| `matrix_runs = 10` | `matrix_list` answers (newest first). Also used by `matrix_status` with no active run: `matrix_run` unset plus the single most recent summary (or an empty list when there is no history) — "nothing is executing", never a fabricated run state. |
| `matrix_preview = 11` | Dry-run answers (`matrix_build`, `matrix_dockerize`, `matrix_resume` with `dry_run=true`). |
| `matrix_docker = 12` | Terminal outcome of `matrix_dockerize` (image builds have no run). |

`status` / `error` / `error_code` / `timestamp` follow the existing command
result conventions (§7).

### 2.4 `MatrixRunState`

The complete state of one Matrix Run — a projection of the agent's persisted
run record (`matrix.Run`), never a second state model.

| Field | Type | Notes |
|---|---|---|
| `matrix_run_id` | string | `mx_YYYYMMDD_xxxx` (4 hex chars, minted by the agent, unique per day, path-safe). |
| `app_name` | string | |
| `project_dir` | string | Absolute path on the agent. |
| `status` | string | Run status — see §3.1. |
| `config` | `MatrixRunConfig` | The run's configuration snapshot (captured at run start, never re-read). |
| `started_at` / `finished_at` | int64 | Unix milliseconds; `finished_at` = 0 while running. |
| `duration_ms` | int64 | 0 while running. |
| `counters` | `MatrixCounters` | Consistent per-status snapshot; `total == succeeded + failed + skipped + running + pending` always holds (computed under the run's lock). |
| `combinations` | `repeated MatrixCombinationState` | One entry per combination, plan order. |
| `parent_run_id` | string | Non-empty for manual-retry runs: the source run. |
| `resume_count` | int32 | How many times the run was resumed. |
| `active` / `lock_pid` | bool / int32 | Live execution: the run's lock file is held by a live process. Populated in status answers and resync snapshots. A `"running"` run with `active=false` is **orphaned** (agent crashed) — resumable, not executing. |
| `release` | `MatrixReleaseInfo` | Present when a release manifest exists (terminal runs with successful artifacts): logical `version` (versions.json vN), `tag`, `status` (`complete`/`partial`), `artifacts` count, `total_combinations`. |

`MatrixRunConfig`: `language` (`go`/`rust`), `versions`, `platforms`,
`concurrency`, `include`/`exclude` (`repeated MatrixRule` — dimension map +
metadata map), `retries`, `build_args`, `sources` (`MatrixRunConfigSources`,
one origin string per dimension).

`MatrixCombinationState`:

| Field | Notes |
|---|---|
| `id` | Path-safe combination ID, e.g. `go1.26-linux-amd64`, `go1.22.4-linux-arm-v7`. |
| `identity` | Run-scoped `"<runID>/<combinationID>"` (native runs; empty in docker results). |
| `toolchain` / `toolchain_version` / `os` / `arch` / `variant` / `platform` | The full dimension set; `platform` = `os/arch[/variant]`. |
| `status` | Combination status — see §3.2. |
| `duration_ms` | Final attempt-set duration; 0 when unknown. |
| `artifact` | Native: absolute binary path. Docker: image reference. |
| `sha256` | Native: SHA-256 of the artifact bytes, lowercase hex (64 chars). Docker: the image's **content digest**. Empty for failed/incomplete combinations — only a verified artifact carries one. A checksum failure fails the combination (never ships an unverifiable artifact). |
| `size_bytes` | File size (native, best-effort stat at conversion). `0` for image references and missing files. |
| `started_at` | When the current/last attempt began (unix ms; 0 = never). |
| `error` | Redacted final error; empty on success. |
| `cache_status` | `cold` / `hit` / empty (compiler-cache classification from toolchain output). |
| `attempts` | Attempt count; 1 when never retried. |
| `attempt_log` | `repeated MatrixAttemptState` (`number`, `status`, `duration_ms`, `error`) — full automatic-retry history in order. |
| `metadata` | Include-rule attributes merged into the combination (e.g. `tag: latest`). |

`MatrixRunSummary` (list view): `matrix_run_id`, `app_name`, `status`,
`counters`, `parent_run_id`, `resume_count`, `started_at`, `finished_at`,
`duration_ms`.

`MatrixPlanPreview` (dry-run answers):

- build/dockerize: `language`, `combinations` (IDs in plan order),
  `base_count`, `included_count`, `excluded_count`.
- resume: `resume_run_id`, `run_total`, `execute_ids` (the subset a resume
  would execute — the run's pending and orphaned-running combinations). The
  plan fields stay empty: a resume executes a subset of an existing plan, not
  a new expansion.

`MatrixDockerResult` (dockerize terminal): `app_name`, `registry`, `tag`,
`version` (recorded versions.json version; 0 when not recorded), `pushed`,
`push_partial`, `multi_arch_images`, `counters`, `combinations`
(`MatrixCombinationState` with image reference + digest each).

### 2.5 Events: `ReportMatrixEvent(MatrixEvent)`

Sent over a dedicated unary RPC (the same delivery model as
rollback/deployment events: one background sender goroutine, a cached
connection, a bounded buffer). **Events are best-effort telemetry**: a full
buffer drops events, and a send failure drops the event. The durable state
surfaces are the terminal command result (ledger-backed) and `matrix_status`
queries. A backend that does not implement the RPC answers `UNIMPLEMENTED`;
the agent continues the build unchanged.

Event types (`event` field):

| Event | When | Payload |
|---|---|---|
| `matrix.started` | A native run began executing (fresh or manual-retry run; lock acquired). | `run` (full initial state), `status`, `counters`. |
| `matrix.resumed` | A resume session began on an existing run. | `run` (state after the resume-count bump), `status`, `counters`. |
| `matrix.combination_started` | One attempt of one combination began. `attempt >= 2` marks an automatic retry. | `combination` (dimensions + `status:"running"` + `attempts`), `attempt`. |
| `matrix.combination_completed` | One combination reached a final outcome. | `combination` — the full result: status, duration, artifact, `sha256`, size, cache, attempts, attempt log, redacted error. |
| `matrix.completed` | A native run reached a terminal status (`succeeded`/`partial`/`failed` — `status` disambiguates). | `run` (full final state incl. release info), `status`, `counters`, `message`. |
| `matrix.interrupted` | A native run stopped early (daemon shutdown/cancellation). Resumable. | `run` (full state), `status` = `interrupted`, `counters`, `message`. |

Docker matrix builds use the same event types minus `matrix.resumed` and
`matrix.interrupted` (no run to resume): `matrix.started` carries `metadata`
(`registry`, `tag`, `push`, `push_partial`) and `counters` with the plan
size; `matrix.completed` carries the aggregate status and counters. `mode` is
`docker` and `matrix_run_id` is empty — events correlate by `request_id`.

`MatrixEvent` fields:

| Field | Notes |
|---|---|
| `server_id` | Agent identity (same as every monitor event). |
| `request_id` | Correlation with the originating command — present on every event. |
| `matrix_run_id` | Native runs; empty for docker. |
| `app_name` | |
| `event` / `mode` / `status` / `message` | See above; `mode` = `native` \| `docker`. |
| `timestamp` | Unix ms. |
| `seq` | **Per-run** (native) / **per-request** (docker) monotonic counter starting at 1. Strictly increasing within that scope; gaps mean dropped events. Never comparable across runs/requests. |
| `counters` | Run-level events only. |
| `combination` / `attempt` | Combination events only. |
| `error` / `error_code` | Terminal events of failed executions. |
| `run` | `matrix.started`/`matrix.resumed`/`matrix.completed`/`matrix.interrupted` only. **Not** on combination events — the backend aggregates from counters and per-combination updates, or re-queries with `matrix_status`. |
| `metadata` | Docker `matrix.started` details. |

Guaranteed on **every** event: `server_id`, `request_id`, `event`,
`timestamp`, `seq`, `mode`. Everything else is event-type-specific as
documented above.

### 2.6 Resync snapshot: `MonitorEvent.matrix_run`

On every MonitorStream (re)connection — and on the 60-second deployment
resync cadence — the agent pushes the full `MatrixRunState` of every
**active** run (persisted status `running` **and** execution lock held by a
live process) as `MonitorEvent` payload `matrix_run = 19`. This is how a
backend that missed events (its own restart, a disconnect during a build)
converges without asking. Runs that are not executing — finished history, or
`running` records orphaned by a crash — are **not** resynced; the backend
learns about them from command results and `matrix_status` queries.

---

## 3. State machines

All states are **strings** (the agent's own vocabulary; an unknown value must
degrade, never break decoding — the established convention of this protocol).
Empty means "unknown/not applicable".

### 3.1 Run status (`MatrixRunState.status`)

```
                ┌──────────────────────────────────────────┐
                │                running                    │
                └───────┬───────────┬───────────┬──────────┘
      all combinations   │           │           │  stopped early
      terminal, some     │           │           │  (daemon shutdown /
      succeeded          │           │           │  cancellation)
            ┌────────────▼──┐   ┌────▼─────┐  ┌─▼──────────────┐
            │   succeeded   │   │  failed  │  │  interrupted   │
            └───────────────┘   └──────────┘  └────────┬───────┘
                  (terminal)      (terminal)   resumable│
                                    terminal statuses   │ matrix_resume
                                                        ▼
                                              back to running
```

- Terminal statuses: `succeeded` (no failures), `partial` (some succeeded,
  some failed), `failed` (nothing succeeded; an empty run is failed).
  **Terminal runs are immutable** in the agent's history store.
- `interrupted` is resumable, not terminal: incomplete combinations (pending
  + orphaned-running) remain and a resume executes exactly those.
- A persisted `running` record without a live lock holder is **orphaned**
  (agent crash / SIGKILL): resumable, not executing. Stale locks (dead PID)
  are reclaimed on resume.

### 3.2 Combination status (`MatrixCombinationState.status`)

```
pending ──► running ──► success
   │           │   └──► failed
   │           │   └──► skipped        (dry-run only; see below)
   └───────────┘
        running ──► (attempt >= 2: automatic retry, back to running)
```

- Terminal: `success`, `failed`, `skipped`.
- `pending`/`running` entries mean "not finished" — they make the run
  resumable. A `running` entry whose worker is gone (interruption) is
  re-executed by a resume; its stale artifact/checksum are cleared first —
  a resumed combination never keeps a stale artifact from an earlier attempt.
- `skipped` appears only in dry-run plan previews and report data; real
  remote executions never skip (there is no per-combination skip mechanism
  other than exclude rules, which remove combinations before execution).

### 3.3 Attempt lifecycle

Attempt 1 is the original execution; attempts 2, 3, … are automatic retries
(only while the failure is classified transient: network, connection,
timeout, unavailable, Docker daemon). The retry budget is `retries`
additional attempts. The full attempt history is preserved in
`attempt_log`; the final attempt's status equals the combination status.
Automatic retries keep the same run and combination; a manual `matrix_retry`
creates a NEW run with `parent_run_id` pointing back — the source run is
immutable.

### 3.4 Recovery model (three distinct mechanisms)

| Mechanism | Scope | What it does |
|---|---|---|
| Automatic retry | Same run, same combination | Extra attempts for transient failures, within the `retries` budget. |
| Resume (`matrix_resume`) | Same run | Executes only pending + orphaned-running combinations from the run's config snapshot; bumps `resume_count`; never rebuilds completed combinations. |
| Manual retry (`matrix_retry`) | New run | Re-executes the failed combinations of a source run in a new linked run (`parent_run_id`), using the source's config snapshot. |

---

## 4. Per-command behavior

### 4.1 `matrix_build`

1. Resolve the app (name or ID) → project directory; load the app's
   `phelix.yaml`; detect the project language.
2. Converge the profile (payload > YAML > default), validate, expand the
   plan (base Cartesian → include → exclude; every rule must match ≥1
   combination at its point, else `INVALID_ARGUMENT`).
3. `dry_run` → `matrix_preview` (plan view). No run, no lock, no builds,
   no state mutation, no events.
4. Otherwise: mint a run ID, snapshot the config, persist the run
   (status `running`, all combinations `pending`) **before** the first build,
   acquire the run lock, emit `matrix.started`, execute
   (per-combination events as attempts start and results land; incremental
   persistence after every attempt start and every completed combination),
   finalize, run `completeMatrixSession` (report.json in the project,
   version record in versions.json with per-artifact checksums, release
   manifest for succeeded/partial runs, build-report regression analysis),
   emit the terminal event, return the terminal result.

Errors: unknown app `NOT_FOUND`; unsupported project language
`UNSUPPORTED_PROJECT`; invalid dimensions/rules `INVALID_ARGUMENT`; run-lock
conflict `DEPLOY_LOCKED`; partial failure → result `status:"error"`,
`error_code:"BUILD_FAILED"`, **with the full run state attached**.

### 4.2 `matrix_dockerize`

Same resolution/convergence; then the docker matrix core: docker daemon
check → Dockerfile/.dockerignore generation (when absent) → buildx check →
per-combination image builds (`{app}:{tag|latest}-{lang}{version}-{arch}
[-{variant}]`) → optional push (fail-closed unless `push_partial`) →
optional per-version multi-arch manifest lists (requires `push`) → version
record with image digests. **No Matrix Run, no run history, no resume, no
release manifest** — identical to the local command. The terminal result
carries `matrix_docker` with per-combination image references and digests.
Events: `matrix.started` (metadata), combination events, `matrix.completed`.

### 4.3 `matrix_resume`

`target` = `"latest"` (newest resumable run, optionally filtered by
`app_name`) or an explicit run ID. Guards: the run must be resumable
(`INVALID_ARGUMENT` otherwise); an explicit `app_name` that disagrees with
the run's app is `INVALID_ARGUMENT`. `dry_run` → preview (`resume_run_id`,
`run_total`, `execute_ids`) with zero mutation (no resume-count bump).
Otherwise: bump `resume_count`, set status `running`, override build args if
the payload carries any, persist, execute the incomplete subset, complete,
terminal result + events (`matrix.resumed` … terminal).

### 4.4 `matrix_retry`

`target` = explicit source run ID. Guards: source has failed combinations
(`INVALID_ARGUMENT` when none), source has a project directory recorded
(runs predating snapshots cannot be retried), app-name guard as above. No
dry-run (rejected). Creates a new run (fresh ID) with the source's config
snapshot, `parent_run_id` set, executes only the failed combinations,
completes, terminal result + events under the new run ID.

### 4.5 `matrix_status`

Read-only. Target semantics:

- `""` / `"active"`: the active run (`app_name` optionally filters when
  several execute). No active run → success with `matrix_run` unset and
  `matrix_runs` = [most recent summary] (or `[]` when there is no history).
- `"latest"`: the most recent run of any status (`app_name` filter applies).
  None → `NOT_FOUND`.
- Explicit ID: that run's full state (`NOT_FOUND` when unknown; `app_name`
  mismatch → `INVALID_ARGUMENT`).

The state reports live-execution flags (`active`, `lock_pid`) and the release
view when a manifest exists. A completed run never reports `active`.

### 4.6 `matrix_list`

Read-only. Newest first; `app_name` optional filter; `list_limit`
(0→20, cap 100). Returns `matrix_runs` summaries. `skipped` counts appear in
the counters where relevant; `incomplete` combinations are folded into
`running`/`pending` by the consistent counter snapshot.

---

## 5. IDs, correlation, and ordering

| ID | Minted by | Format | Scope |
|---|---|---|---|
| `request_id` | Backend | opaque non-empty string | one logical command; reused verbatim on retries |
| `matrix_run_id` | Agent | `mx_YYYYMMDD_xxxx` (4 hex) | one native matrix run; survives resume; new for manual retry |
| combination `id` | Agent (derived from dimensions) | `{lang}{version}-{os}-{arch}[-{variant}]` | stable, path-safe; unique within a plan |
| combination `identity` | Agent | `<runID>/<combinationID>` | unambiguous across runs |
| `attempt` | Agent | integer ≥ 1 | per combination within a run |
| `seq` | Agent | integer ≥ 1, monotonic | per run (native) / per request (docker); gaps = dropped events |

Correlation rules:

- Every event carries `request_id` and (native) `matrix_run_id`; the
  terminal result carries `matrix_run_id` and the full state. The backend can
  join command ↔ events ↔ status on either ID.
- **The backend must be idempotent by `request_id` and must never mint a new
  `request_id` when retrying a command** (same rule as remote rollback). The
  agent deduplicates by `request_id` only — a same-payload command with a new
  `request_id` executes a second time (that is a feature: "build again" is a
  new command).
- gRPC send success ≠ backend acknowledgement: treat every result as
  at-least-once and deduplicate by `request_id`.

Event ordering: within one run/request, events are totally ordered by `seq`
— order events by `seq`, not by arrival. Events for *different* combinations
are emitted from concurrent worker goroutines and may arrive swapped
(harmless: they are independent); events for one combination, and the
run-level `started`/`resumed`/terminal events, are emitted from a single
goroutine in causal order. Timestamps are unix-ms and monotonic within a run
but may tie. Cross-run ordering is not guaranteed.

---

## 6. Idempotency and the command ledger

Mutating matrix commands are guarded by a durable ledger at
`<agent data dir>/remote-matrix-ledger.json` — the same mechanism as remote
rollback, in a separate file (a corrupt or exhausted matrix ledger never
affects rollback idempotency).

Rules:

1. **`begin` (before execution):** the entry (`request_id`, payload
   fingerprint = SHA-256 of the deterministic proto marshal with
   `request_id` cleared, command type, state `in_progress`) is persisted
   atomically (temp file → fsync → 0600 → rename → dir fsync) before
   execution starts.
2. **Same `request_id` + same payload:** while in progress → immediate
   `UNAVAILABLE` ("already in progress") result, never a second execution;
   after completion → **replay** of the stored terminal result (delivered
   again, then marked delivered).
3. **Same `request_id` + different payload:** `ALREADY_EXISTS` — a backend
   bug, rejected loudly.
4. **Capacity:** 256 entries; full → `UNAVAILABLE` (nothing is evicted).
5. **`complete` (after execution, before the send attempt):** the terminal
   result is persisted with the entry. A crash after execution but before
   delivery replays the result on the next connection.
6. **Agent restart with an entry in progress:** the entry converts to a
   terminal `UNAVAILABLE` "indeterminate" result — the command is **never
   re-executed**. The result message points at the durable surfaces: any
   matrix run the command created is persisted with its per-combination
   state and can be inspected (`matrix_status`/`matrix_list`) and continued
   (`matrix_resume`) by its run ID. The run itself finalizes as
   `interrupted` when the daemon shuts down gracefully (see §8); only a
   SIGKILL leaves it orphaned-`running`, still resumable.
7. **Corrupt/unreadable ledger:** daemon startup fails closed — matrix
   commands answer `UNAVAILABLE` rather than executing without idempotency.
8. Queries are **not** ledgered: a query has no execution to make
   idempotent, and the backend can re-ask freely.

Delivery/replay: on every MonitorStream (re)connection the agent re-sends
ledger results that were recorded but not confirmed delivered, oldest first,
then marks them delivered. Results are re-sent until the stream accepts
them; the backend must tolerate duplicates (dedupe by `request_id` + result
timestamp).

---

## 7. Failure and error semantics

- Result `status` is `"success"` or `"error"`, per the existing command
  result convention. On error, `error_code` carries a Phelix error code
  (`internal/errors/codes.go`); `error` is a human-readable, **redacted**
  message. Never parse `error` to derive meaning — use `error_code`.
- Codes a backend should expect: `INVALID_ARGUMENT` (bad request shape,
  invalid dimensions/rules, guards), `NOT_FOUND` (unknown app or run),
  `ALREADY_EXISTS` (ledger fingerprint conflict), `UNAVAILABLE`
  (in-progress duplicate, ledger unavailable/full, indeterminate restart
  outcome), `DEPLOY_LOCKED` (run lock held by another live process),
  `UNSUPPORTED_PROJECT`, `BUILD_FAILED` (combination failures — see below),
  `DOCKER_ERROR` / `DOCKER_DAEMON_UNAVAILABLE` (dockerize path),
  `UNKNOWN` (unclassified, incl. recovered panics), `UNIMPLEMENTED` (no
  handler registered — startup misconfiguration).
- **A matrix run with failed combinations is a completed execution, not a
  transport error**: the result is `status:"error"` + `error_code:
  "BUILD_FAILED"` **and** carries the full run state (`matrix_run` with
  `partial`/`failed` status, per-combination outcomes, artifacts and
  checksums of the successful set, partial release manifest). An interrupted
  run uses the same error code with run status `interrupted` and a
  resume-pointer message — mirror the message texts, but branch on
  `matrix_run.status`, not on message parsing.
- Per-combination errors on the wire are already redacted by the engine
  (secrets/tokens scrubbed; compiler output fragments bounded). Docker image
  builds fail the combination when the artifact cannot be verified.
- Events carry `error`/`error_code` only on terminal events of failed
  executions.

Sensitive data: no tokens, session material, or SSH credentials ever appear
in matrix fields (auth rides gRPC metadata, not bodies). Error strings are
redacted at the engine boundary; handler-level errors are redacted at result
finalization.

---

## 8. Reconnect, restart, and shutdown expectations

**Backend disconnects mid-build:** the build keeps running (execution is
agent-side). Events during the disconnect are dropped (best-effort); the
terminal result is ledger-persisted and replayed on the next MonitorStream
connection. While the run is active, the reconnect resync
(`MonitorEvent.matrix_run`) delivers its full state; `matrix_status` answers
queries at any time.

**Backend restarts:** same as above — it converges via the replayed terminal
results, the resync snapshot, and queries.

**Agent (daemon) restarts mid-build (graceful SIGTERM):** in-flight matrix
sessions are canceled *first*: no new combinations start, in-flight builds
are killed, their combinations stay pending, the run finalizes
`interrupted` (resumable), and the terminal result (BUILD_FAILED,
`interrupted` state) is persisted and delivered. The daemon then drains
in-flight commands (15s bound) and flushes the event sender (5s bound)
before closing the connection.

**Agent crash (SIGKILL):** the ledger entry converts to the indeterminate
`UNAVAILABLE` result (never re-executes). The run record stays
orphaned-`running`; a later `matrix_resume` reclaims the stale lock and
continues.

**Run locks:** one execution per run at a time (lock file with owning PID;
dead-PID locks are reclaimed on resume). Different runs may execute
concurrently — a backend may issue concurrent matrix builds for different
apps (or different runs of the same app); resource contention on the agent
is the operator's concern, exactly as with concurrent local builds.

**Known same-as-local limitations** the backend should model: per-project
`builds/matrix/report.json` is rewritten by the latest session (two
concurrent runs of the same app race on it, last write wins); a resumed run
re-records a new application version and regenerates its manifest (last
write wins); dockerize matrix has no run/resume; verification-free — there
is no post-build health verification in the matrix engine.

---

## 9. Persistence expectations (agent side, for reference)

- Run records: `<PHELIX_DATA_DIR>/matrix/runs/<run-id>.json` — created before
  the first build, updated incrementally (every attempt start, every
  completed combination; atomic temp+rename; terminal runs immutable).
- Release manifests: `<runs dir>/<run-id>.manifest.json` (atomic overwrite on
  resume; only terminal runs with successful artifacts).
- Run locks: `<runs dir>/<run-id>.lock` (owning PID).
- Command ledger: `<PHELIX_DATA_DIR>/remote-matrix-ledger.json` (§6).
- Version records: versions.json under the app's data dir — one
  `VersionMeta` row per matrix build with N `MatrixArtifacts`
  (`matrix_run_id` on each), retention-capped.
- report.json: `<project>/builds/matrix/report.json` (run-level view).

The backend should treat these as the agent's internal state: the wire
already exposes everything the backend needs.

---

## 10. Backward compatibility

- All proto changes are additive (new fields 8–12 + `matrix = 10` on
  existing messages, new oneof case 19, new RPC). Older backends ignore
  unknown fields/cases; older agents never populate them.
- `ReportMatrixEvent` may answer `UNIMPLEMENTED` on older backends — the
  agent logs and continues (same policy as `ReportDeploymentEvent`).
- The new command types only reach agents that registered the matrix
  handler; a backend must not send `matrix_*` commands to agents that
  predate this phase (they would be routed to the unknown-type lifecycle
  fallback and fail confusingly). Backend-side gating by agent version is
  the Phase 2 responsibility.
- Local CLI matrix behavior is unchanged: the same engine, the same flags,
  the same outputs. `startMatrixSession` (signal handling) and
  `runDockerizeMatrixMode` (flag wiring) are now thin wrappers over the
  shared cores the remote path uses.

---

## 11. Examples

### 11.1 Trigger a native matrix build

```json
// Backend → agent, MonitorControl.command
{
  "request_id": "b-2026-09-10-0001",
  "type": "matrix_build",
  "app_name": "payments-api",
  "matrix": {
    "go_versions": ["1.22", "1.23"],
    "platforms": ["linux/amd64", "linux/arm64"],
    "concurrency": 4,
    "retries": 1,
    "tag": "v2.3.0"
  }
}
```

Terminal result (success):

```json
// Agent → backend, MonitorEvent.command_result
{
  "request_id": "b-2026-09-10-0001",
  "command": "matrix_build",
  "app_name": "payments-api",
  "status": "success",
  "timestamp": 1789035000000,
  "matrix_run_id": "mx_20260910_ab12",
  "matrix_run": {
    "matrix_run_id": "mx_20260910_ab12",
    "app_name": "payments-api",
    "status": "succeeded",
    "config": {
      "language": "go",
      "versions": ["1.22", "1.23"],
      "platforms": ["linux/amd64", "linux/arm64"],
      "concurrency": 4, "retries": 1,
      "sources": {"language": "cli", "versions": "cli", "platforms": "cli",
                   "concurrency": "cli", "retries": "cli"}
    },
    "started_at": 1789034940000, "finished_at": 1789035000000,
    "duration_ms": 60000,
    "counters": {"total": 4, "succeeded": 4, "failed": 0, "skipped": 0,
                  "running": 0, "pending": 0},
    "combinations": [
      {
        "id": "go1.22-linux-amd64",
        "identity": "mx_20260910_ab12/go1.22-linux-amd64",
        "toolchain": "go", "toolchain_version": "1.22",
        "os": "linux", "arch": "amd64", "platform": "linux/amd64",
        "status": "success", "duration_ms": 14200,
        "artifact": "/srv/payments-api/builds/matrix/go1.22-linux-amd64/payments-api_amd64_go_1.22",
        "sha256": "9f2b…(64 hex chars)",
        "size_bytes": 8421, "attempts": 1,
        "attempt_log": [{"number": 1, "status": "success", "duration_ms": 14200}],
        "cache_status": "cold"
      }
      // … three more combinations
    ],
    "release": {"version": 7, "tag": "v2.3.0", "status": "complete",
                 "artifacts": 4, "total_combinations": 4}
  }
}
```

### 11.2 Lifecycle events during the build

```json
{"server_id": "agent-7d…", "request_id": "b-2026-09-10-0001",
 "matrix_run_id": "mx_20260910_ab12", "app_name": "payments-api",
 "event": "matrix.started", "timestamp": 1789034940010, "seq": 1,
 "mode": "native", "status": "running",
 "counters": {"total": 4, "pending": 4},
 "run": { /* full MatrixRunState, all combinations pending */ }}

{"event": "matrix.combination_started", "seq": 2, "timestamp": 1789034940020,
 "attempt": 1,
 "combination": {"id": "go1.22-linux-arm64", "toolchain": "go",
                  "toolchain_version": "1.22", "os": "linux", "arch": "arm64",
                  "platform": "linux/arm64", "status": "running", "attempts": 1}}

{"event": "matrix.combination_completed", "seq": 3, "timestamp": 1789034954400,
 "combination": {"id": "go1.22-linux-arm64", "status": "success",
                  "duration_ms": 14380, "artifact": "…/payments-api_arm64_go_1.22",
                  "sha256": "1c8e…", "size_bytes": 8390, "attempts": 1,
                  "attempt_log": [{"number": 1, "status": "success", "duration_ms": 14380}],
                  "cache_status": "cold"}}

// A transient failure retried automatically:
{"event": "matrix.combination_started", "seq": 7, "attempt": 2, "…": "…"}
{"event": "matrix.combination_completed", "seq": 8,
 "combination": {"id": "go1.23-linux-amd64", "status": "success", "attempts": 2,
                  "attempt_log": [{"number": 1, "status": "failed", "error": "…"},
                                   {"number": 2, "status": "success"}]}}

{"event": "matrix.completed", "seq": 10, "status": "succeeded",
 "counters": {"total": 4, "succeeded": 4},
 "run": { /* full final MatrixRunState incl. release */ }}
```

### 11.3 Partial failure

```json
{"request_id": "b-2026-09-10-0002", "command": "matrix_build",
 "status": "error",
 "error": "matrix run mx_20260910_cd34 completed with 1 failure(s) out of 4 combinations — retry them with a matrix_retry command (target mx_20260910_cd34)",
 "error_code": "BUILD_FAILED",
 "matrix_run_id": "mx_20260910_cd34",
 "matrix_run": {"status": "partial",
                "counters": {"total": 4, "succeeded": 3, "failed": 1},
                "release": {"version": 8, "status": "partial", "artifacts": 3, "…": "…"},
                "…": "…"}}
```

### 11.4 Dry-run preview

```json
// Request: type=matrix_build, dry_run=true, matrix.go_versions=["1.22"],
//          matrix.platforms=["linux/amd64","windows/amd64"]
{"status": "success",
 "matrix_preview": {"language": "go",
                     "combinations": ["go1.22-linux-amd64", "go1.22-windows-amd64"],
                     "base_count": 2, "included_count": 0, "excluded_count": 0}}
```

### 11.5 Resume after an interruption

```json
// Request: type=matrix_resume, target="mx_20260910_cd34", request_id="b-…-0003"
// Result:
{"status": "success", "matrix_run_id": "mx_20260910_cd34",
 "matrix_run": {"status": "succeeded", "resume_count": 1,
                "parent_run_id": "",
                "counters": {"total": 4, "succeeded": 4},
                "release": {"version": 9, "status": "complete", "…": "…"},
                "…": "…"}}
// Events began with:
{"event": "matrix.resumed", "matrix_run_id": "mx_20260910_cd34", "seq": 1,
 "status": "running", "run": { /* resume_count: 1 */ }}
```

### 11.6 Status query

```json
// Request: type=matrix_status, target="" (active), app_name="payments-api"
{"status": "success", "matrix_run_id": "mx_20260910_ab12",
 "matrix_run": {"status": "running", "active": true, "lock_pid": 3481,
                "counters": {"total": 4, "succeeded": 1, "running": 2, "pending": 1},
                "…": "…"}}

// No active run:
{"status": "success",
 "matrix_runs": [{"matrix_run_id": "mx_20260910_ab12", "status": "succeeded",
                   "counters": {"total": 4, "succeeded": 4}, "…": "…"}]}
```

### 11.7 Docker matrix (images)

```json
// Request: type=matrix_dockerize, app_name="payments-api",
//          matrix={go_versions:["1.22"], platforms:["linux/amd64","linux/arm64"],
//                   tag:"v2", registry:"ghcr.io/acme", push:true, multi_arch_tag:true}
{"status": "success",
 "matrix_docker": {"app_name": "payments-api", "registry": "ghcr.io/acme",
                    "tag": "v2", "version": 10, "pushed": true,
                    "multi_arch_images": ["ghcr.io/acme/payments-api:v2-1.22"],
                    "counters": {"total": 2, "succeeded": 2},
                    "combinations": [
                      {"id": "go1.22-linux-amd64", "status": "success",
                       "artifact": "ghcr.io/acme/payments-api:v2-go1.22-amd64",
                       "sha256": "sha256:…digest…", "size_bytes": 0, "…": "…"},
                      {"id": "go1.22-linux-arm64", "…": "…"}]}}
// Events: matrix.started with mode="docker", metadata={registry, tag, push},
// combination events, matrix.completed — matrix_run_id empty throughout.
```

### 11.8 Duplicate request (backend timeout + retry)

```json
// Same request_id, same payload, after completion:
{"request_id": "b-2026-09-10-0001", "status": "success",
 "matrix_run_id": "mx_20260910_ab12", "…": "…"}   // replayed stored result

// Same request_id while still executing:
{"status": "error", "error_code": "UNAVAILABLE",
 "error": "matrix_build request is already in progress"}

// Same request_id, different payload:
{"status": "error", "error_code": "ALREADY_EXISTS",
 "error": "request_id already exists with a different matrix_build payload"}
```

---

## 12. Implementation map (agent side)

| Concern | Where |
|---|---|
| Proto contract | `internal/grpc/proto/matrix.proto`, `monitoring.proto`, `phelix.proto` |
| Command dispatch, validation, ledger, async execution | `internal/grpc/matrix_command.go` |
| Ledger (shared with rollback) | `internal/grpc/rollback_command.go` |
| Event reporter + sender | `internal/grpc/matrix_reporter.go` |
| Run/result → wire conversions | `internal/grpc/matrix_convert.go` |
| Stream wiring (dispatch, replay, resync) | `internal/grpc/monitor_stream.go` |
| Remote handlers (engine reuse) | `cmd/matrix_remote.go` |
| Session core (ctx + observer) | `cmd/matrix_exec.go` |
| Docker matrix core | `cmd/dockerize.go` |
| Daemon wiring (ledger init, handler, shutdown drain) | `cmd/monitor.go` |
| Tests | `internal/grpc/matrix_command_test.go`, `matrix_convert_test.go`, `matrix_reporter_test.go`; `cmd/matrix_remote_test.go` |
