# Machine Contract

Every core command accepts `--json` and emits a single, versioned JSON
envelope on **stdout**. This is the contract coding agents and scripts
consume. It is additive-only: breaking changes bump `schema_version`.

The matrix (`matrix list/show/status`) and webhook (`webhook status/history`)
JSON outputs predate this contract and keep their own schemas for backward
compatibility.

## Envelope

```json
{
  "schema_version": "1",
  "operation_id": "op_1a2b3c4d5e6f7081",
  "status": "succeeded",
  "result": {},
  "error": null
}
```

| Field | Presence |
|---|---|
| `schema_version` | always |
| `operation_id` | mutation commands only — the durable operation identifier. Read-only commands never fabricate one. |
| `status` | `"succeeded"` on success, `"failed"` on failure |
| `result` | success only; shape documented per command below |
| `error` | failure only; see [errors](#errors) |

## stdout / stderr discipline

- **stdout**: the envelope, nothing else — no banners, progress, colors,
  warnings, or debug output.
- **stderr**: progress and diagnostics (what a non-JSON run prints to stdout).

Exit codes are unchanged by `--json`.

## Commands

| Command | `operation_id` | result |
|---|---|---|
| `build` | `op_…` (matrix: the `mx_…` run ID) | `{app, app_id, version, path}` / matrix: `{run_id, app, total, succeeded, failed, skipped, report_path}` |
| `rebuild` | `op_…` (also correlated with the `dep-…` deployment ID) | `{app, app_id, version, port, strategy}` |
| `rollback` | `op_…` | `{app, from_version, to_version, strategy, port, verification}` |
| `rollback --dry-run` / `--list` | — | plan view / `{app, count, versions[]}` |
| `deploy unlock` | — (lock management) | `{app, unlocked}` |
| `status <app>` | — | `{app{}, version?, deploy?, proxy{}}` |
| `list` | — | `{apps[], proxy_up, proxy_enrolled_apps}` |
| `doctor` | — | `{checks[], summary{}}` (exit 3 on failing checks) |
| `health list` / `health status` | — | endpoints / check results |
| `log [app] --json` | — | `{source, app, path, items[], count, truncated}` |
| `operation status <id>` / `operation list` | the queried id | operation view / `{operations[], count}` |
| `inspect project` / `runtime` / `config` | — | targeted inspection (see below) |
| `inspect app/deployment/versions/health <app>` | — | app-scoped targeted inspection |
| `inspect capabilities` / `operations` | — | registry + bounded operation list |
| `context [app]` | — | one bounded composite snapshot |
| `plan create rebuild/rollback` | — (the plan itself is the artifact) | immutable execution plan |
| `plan show <plan-id>` | — | plan content + fresh applicability |
| `plan list` | — | `{plans[], count}` |
| `plan apply <plan-id>` | the produced `op_…` | execution result + correlation |

## Plans (Phase 3)

Plans are first-class, immutable, content-addressed execution artifacts. The
invariant: **the thing that was planned is the thing that gets executed, or
execution fails closed.**

### Plan schema

```json
{
  "plan_id": "pln_1a2b3c4d5e6f7081",
  "plan_hash": "sha256:…",
  "schema_version": "1",
  "created_at_ms": 0,
  "status": "created",
  "action":     { "type": "rebuild|rollback", "application": "…" },
  "target":     { "app_id": "…", "app_name": "…", "language": "…" },
  "inputs":     { "source_dir": "…", "port": 0, "tag": "…", … },
  "execution":  { "strategy": "…", "steps": ["…"], … },
  "preconditions": [ { "type": "…", "expected": "…", "source": "…" } ],
  "capabilities": ["deployment"],
  "correlation": { "operation_id": "op_…", "deployment_id": "dep-…" },
  "applicability": { "state": "applicable|stale", "checked_at_ms": 0,
                     "failed_preconditions": [], "missing_capabilities": [] }
}
```

### Plan identity

- **plan_id**: `pln_` + 16 hex chars, minted at creation, durable.
- **plan_hash**: `sha256:` over a canonical preimage of EXACTLY the
  execution-relevant fields (schema_version, action, target, inputs,
  execution, preconditions, capabilities). Deliberately excluded:
  `plan_id`, `created_at_ms`, `status`, `correlation` — two plans differing
  only in metadata hash identically. The preimage is a typed struct
  serialized with `encoding/json` (deterministic field order); collections
  inside it are sorted at creation (Canonicalize), so no map ordering can
  leak in. Every load recomputes and verifies the hash — a mismatch fails
  closed with `PLAN_HASH_MISMATCH` before anything is inspected further.

### Lifecycle and immutability

`created → applied | failed` are the only stored statuses; **staleness is
never stored** — it is computed from current state at show/apply time. Plan
files are created exactly once (atomic create-only write under
`<data-dir>/plans/`); re-saving is `ALREADY_EXISTS`. There is no plan edit:
a different plan means a new plan. Status transitions verify the semantic
hash before and after, so they can never smuggle in an execution change.

### Preconditions (what makes a plan stale)

Typed, narrowly scoped facts re-derived at apply time:

| Type | Checked against |
|---|---|
| `app_exists` | the app still exists with the same ID |
| `current_version` | the currently deployed version is unchanged |
| `version_exists` | the target artifact still exists (retention!) |
| `deploy_mode` | the deployment mode (classic/blue-green/rolling) is unchanged |
| `config_fingerprint` | execution-relevant config (deploy block, resources, ports) hashes to the planned value |
| `source_commit` | the git commit of the source is unchanged |
| `toolchain_available` | the required toolchain is still installed |

The config fingerprint deliberately excludes logs, timestamps, health and
metrics: a plan goes stale because **execution semantics changed**, not
because an unrelated log line appeared. Unknown precondition types fail
closed (`unchecked`).

### Apply semantics

```text
plan apply
  → load + parse (PLAN_CORRUPT on garbage) + schema (PLAN_INVALID) + hash
    (PLAN_HASH_MISMATCH) — all fail closed
  → status already applied → return the recorded correlation (no mutation)
  → evaluate preconditions → any failure = PLAN_STALE, exit 3, NO MUTATION
  → evaluate capabilities  → missing   = PLAN_CAPABILITY_MISSING, exit 3
  → re-resolve execution inputs through the SAME resolver the command uses;
    any drift from the plan = PLAN_STALE
  → execute through the existing engine (rebuild path / rollback engine)
  → mark plan applied|failed; correlate plan_id + plan_hash ↔ operation_id
    ↔ deployment_id
```

There is no silent replanning: an agent that wants a new plan must create
one explicitly.

### Idempotency

Plan application reuses the Phase 1 request-key ledger with the derived key
`pln:<plan-id>` (fingerprint = the plan hash):

1. **First apply** — executes; the terminal envelope is recorded.
2. **Repeat after success** — the stored correlation is returned
   (`already_applied: true`), never a second mutation.
3. **Concurrent apply** — in-process applies serialize; exactly one
   execution. Cross-process duplicates are rejected `UNAVAILABLE` while
   one is in flight.
4. **Apply after process restart** — an interrupted key replays its
   indeterminate result; the plan is never re-executed under an unknown
   outcome.
5. **Apply after failure** — the recorded failure envelope replays
   deterministically; create a NEW plan to retry.
6. **Apply after staleness** — `PLAN_STALE`, no mutation, no regeneration.

### Security

Plans never contain secrets: the schema has no environment-value fields —
rebuilds inject env at execution time from the encrypted store, and the
rollback reason is the only free-text field (validated and redacted as in
Phase 1). Plan JSON passes through the same envelope/redaction chokepoints
as every other machine response.


## Errors

```json
{
  "schema_version": "1",
  "operation_id": "op_1a2b3c4d5e6f7081",
  "status": "failed",
  "error": {
    "code": "DEPLOY_FAILED",
    "message": "…redacted…",
    "exit_code": 21,
    "retryable": true,
    "operation_id": "op_1a2b3c4d5e6f7081"
  }
}
```

- `code` — the stable structured error category (`internal/errors/codes.go`).
- `message` — redacted through the centralized redactor.
- `exit_code` — the exit code the process actually returns.
- `retryable` — omitted when the failure cannot be classified (`UNKNOWN` or
  plain errors). Classification is shared with deployment telemetry
  (`internal/errors/retryable.go`).
- `operation_id` — present when the failure aborted a registered operation.

Usage errors (unknown flags, missing args) occur before a command runs and
are rendered human-readable on stderr with exit 2 — they never produce JSON.

## Lifecycle vocabulary

External statuses are always one of:

```text
pending | running | succeeded | failed | cancelled
```

Internal state machines map into this vocabulary without losing detail: the
operation view reports `status` (external) next to `internal_status` (the
subsystem's own state). `rolled_back`, `partial` and `interrupted` map to
`failed` — an automation consumer must never mistake them for success.

## Operation identity

Every mutation operation gets a durable, queryable identity:

| Workflow | Identity |
|---|---|
| classic/zero-downtime rebuild | `op_…` record correlating the `dep-…` deployment ID |
| rollback | `op_…` record (events carry it as `request_id`) |
| plain build | `op_…` record with the produced version |
| matrix build | the existing `mx_…` run record — no second ID |
| webhook execution | the existing `wh_…` job record — no second ID |

`operation status <id>` resolves any of these prefixes to current state,
result and structured error. `operation list [--app X] [--limit N]` lists
recorded operations newest-first. Operation records are best-effort: a
record-write failure never blocks the mutation itself (the envelope then
omits `operation_id`), while the idempotency ledger below is fail-closed.

## Request-key idempotency

`rebuild` and `rollback` accept `--request-key <key>`.

Semantics:

1. **First use** — the key is recorded durably (atomic fsync'd JSON under
   `<data-dir>/ops/request-key-ledger.json`) with a fingerprint of the
   operation-defining inputs (strategy, replicas, canary, port, source dir,
   tag / target version, verification window). The mutation executes and its
   terminal envelope is stored.
2. **Same key, same inputs** — the stored envelope is replayed byte-for-byte
   and the mutation does **not** execute again. This holds across process
   restarts.
3. **Same key, different inputs** — rejected with `IDEMPOTENCY_CONFLICT`
   (exit 1). The original result stands; use a new key.
4. **Concurrent duplicate** — the second caller is rejected with
   `UNAVAILABLE` while the first is still running.
5. **Interrupted execution** — a key still `in_progress` after a process
   restart is closed as **indeterminate**: its result is an
   `UNAVAILABLE` error envelope and the mutation is never re-executed under
   that key. Inspect the operation, then use a new key if a retry is truly
   safe.

Failures are consumed like successes: replaying a failed key replays the
failure envelope. The ledger is fail-closed — a corrupt ledger answers
`UNAVAILABLE` for every keyed operation rather than executing without
idempotency — and evicts its oldest completed entries when full (in-progress
entries are never evicted).

`build` deliberately does not accept a request key: a build is naturally
repeatable and version-minting, and matrix builds carry their own
resume/retry lineage.

## Bounded logs

`phelix log [app]` gains:

- `--lines N` — history window (default 10, as before).
- `--no-follow` — print history and exit (the human default still tails).
- `--since <dur\|RFC3339>` — drop lines older than the cutoff (timestamped
  log lines only; untimestamped lines are conservatively kept).
- `--json` — a bounded snapshot (`{source, app, path, items[], count,
  truncated}`); incompatible with following by design.

All displayed log lines — human or JSON — pass through the centralized
redactor: credential-shaped content is masked on display. Backend
`[REDACTED:*]` sentinels are ordinary text, never parsed or un-redacted.

## Inspection & context (Phase 2)

`phelix inspect <topic> --json` answers one question;
`phelix context [app] --json` composes the sections into one bounded snapshot.
All sections are projections over the state Phelix already maintains — facts
only, never recommendations (decision-making belongs to a future layer).

| Section | Fields (selected) | Source | Source freshness |
|---|---|---|---|
| `project` | root, name, language, config_path, config_found, config_valid, config_error, port, reads_port, hardcoded_port | project loader + doctor primitives | on-disk |
| `runtime` | os, architecture, go/rust/docker `{installed, version}` | toolchain lookups + docker availability check | live |
| `config` | name, port, deploy `{strategy, replicas, runtime, network}`, resources, health endpoints, watching, matrix, webhook `{branch, secret_env}` | phelix.yaml (selected fields, never a dump) | on-disk |
| `application` | reconciled status, pid, resources, version, deploy mode, replicas, proxy | status reconciliation path | **live** (`source: "live"`) |
| `deployment` | deployment_id, status, strategy, active_version, op_lock, canary, last_rollback, serving_alive | deploy.json | **persisted** (`source: "persisted"`); `serving_alive` is the one live fact |
| `versions` | current + bounded available list | versions.json | on-disk |
| `health` | status: `healthy\|degraded\|unhealthy\|unknown`, checks sorted by name, deploy tier | running daemon or one-shot checker (`source` field says which) | live probe |
| `capabilities` | the agent build's capability registry (sorted) | capability registry | build-level |
| `operations` | Phase 1 operation records, newest first | operation records | persisted |
| `logs` | bounded, redacted lines + `count`/`truncated` | app/self log file | on-disk |

Bounds and semantics:

- Defaults: versions 20, operations 20, log lines 100. Flags
  (`--limit`, `--versions`, `--operations`, `--log-lines`) may lower them;
  values above the caps (100/100/1000) are rejected with `INVALID_ARGUMENT`
  rather than clamped.
- Every bounded section carries `truncated`; the composite carries a
  top-level `truncated` that is true when ANY section dropped data — a false
  there means the context is complete.
- `deployment.status` maps onto the operation lifecycle vocabulary: an
  in-flight op lock is `running`, a persisted topology is `succeeded`, no
  deploy state is `pending`. Health uses its own vocabulary
  (`healthy/degraded/unhealthy/unknown`) because it is not an operation.
- Secrets are structurally absent: the webhook section carries only the NAME
  of the env var holding the HMAC secret; log and health-failure text passes
  through the centralized redactor.
- App-scoped sections are included only when an app is named; the composite
  never guesses which app is meant.

## Actor metadata

Operation records and views include:

```json
{ "actor": { "type": "cli", "id": null, "authenticated": false } }
```

This is provenance metadata, **not** authenticated identity — Phase 1 has no
authenticated actor model and deliberately accepts no forgeable
`--actor` value. A later security phase may populate it from real
credentials.
