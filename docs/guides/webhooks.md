# Git Webhook Deploys

`phelix webhook` starts a long-running HTTP server that accepts Git push webhooks
and deploys the **exact commit that was pushed**. The webhook layer is a
**trigger/orchestration layer only**: it fetches the pushed commit and prepares
an isolated source for it, then hands off to the existing `phelix rebuild`
pipeline, so the deployment strategy from `phelix.yaml` (classic, blue-green,
rolling, canary, progressive), health verification, versioning, rollback history,
build reports and the per-app deploy lock all behave exactly as if you had run
the rebuild yourself.

```text
Git push
  → webhook server: HMAC verification → branch validation → delivery dedup
  → build queue (per-app, serialized)
  → exact-commit fetch from the app's Git remote
  → isolated Git worktree at the pushed commit
  → phelix rebuild --source-dir <isolated source>
  → existing deployment pipeline (build → deploy → health → reports)
```

The deployment target is always the pushed commit SHA — never the branch's
current tip (it may have moved again) and never whatever happens to be in the
server's working tree. Version metadata records the commit that was actually
compiled. Every accepted delivery also becomes a durable, queryable deployment
job (see below).

## Configuration

Each application opts in through its own `phelix.yaml`:

```yaml
webhook:
  enabled: true
  branch: main                 # bare branch name; pushes to other branches are ignored
  secret_env: PHELIX_WEBHOOK_SECRET   # env var holding the shared secret
```

- The shared secret is **never stored in phelix.yaml** and never logged — only
  the name of the environment variable that holds it.
- `phelix webhook` fails at startup with a clear error when an app enables the
  webhook but its `secret_env` variable is not set.
- Projects without a `webhook` section are unaffected.

See the [configuration reference](../reference/configuration.md) for the field
table.

## Running the server

```bash
PHELIX_WEBHOOK_SECRET=... phelix webhook                 # 127.0.0.1:9746
PHELIX_WEBHOOK_SECRET=... phelix webhook --host 0.0.0.0 --port 9746
```

The server listens on `127.0.0.1:9746` by default (`--host`/`--port` to change).
It runs in the foreground, exits cleanly on `SIGINT`/`SIGTERM`, and is designed
to be supervised by systemd like `phelix monitor` (set the secret via
`Environment=` or an `EnvironmentFile`). Apps are resolved live from `apps.json`,
so apps added or removed while the server runs are picked up without a restart.

## Endpoint

```http
POST /webhook/<app>
```

`<app>` is the app name or ID. GitHub-style headers are required:

| Header | Meaning |
|--------|---------|
| `X-Hub-Signature-256: sha256=<hex>` | HMAC-SHA256 of the **exact raw request body** with the shared secret (compared constant-time) |
| `X-GitHub-Delivery: <id>` | Unique delivery ID, used for replay protection |
| `X-GitHub-Event: push` | Event type (`ping` and other events are acknowledged but ignored) |

The request body is the provider's push payload; only `ref` and `after` (the
pushed commit SHA) are read. Refs are normalized (`refs/heads/main` → `main`) and
only the configured `webhook.branch` triggers a rebuild. Branch mismatch, branch
deletion and non-push events are **not** errors — the server answers success
without queueing anything.

Responses are uniform JSON and never expose internal errors, paths or secrets:

| Situation | Status | Body |
|-----------|--------|------|
| Accepted and queued | `200` | `{"accepted":true,"queued":true,"message":"webhook accepted"}` |
| Branch mismatch / deleted / ping | `200` | `{"accepted":true,"queued":false,"message":"branch does not match configured branch"}` |
| Duplicate delivery | `200` | `{"accepted":true,"queued":false,"message":"delivery already processed"}` |
| Invalid/missing/malformed signature | `401` | `{"accepted":false,"queued":false,"message":"invalid signature"}` |
| Unknown app or disabled webhook | `404` | `{"accepted":false,"queued":false,"message":"unknown application"}` |
| Unreadable payload / missing delivery id | `400` | `{"accepted":false,"queued":false,"message":"invalid payload"}` |
| Body over 2 MiB | `413` | `{"accepted":false,"queued":false,"message":"request body too large"}` |
| Queue full/unavailable (provider redelivers) | `503` | `{"accepted":false,"queued":false,"message":"webhook queue unavailable"}` |

## Replay protection and the build queue

- Every accepted delivery ID is recorded in a durable ledger
  (`~/.phelix/webhook/deliveries.json`, bounded to the 512 most recent deliveries)
  **before** the request is acknowledged, so a redelivery — concurrent or after a
  restart — never triggers a second rebuild. The ledger entry also stores the
  pushed commit SHA, so delivery metadata can never be replaced by a later branch
  tip.
- The HTTP response returns as soon as the job is queued; it never waits for the
  fetch or the rebuild.
- Jobs for the same app run strictly one at a time, each deploying **its own**
  commit; different apps run independently. Jobs are never coalesced and an
  accepted job's commit is never replaced with a newer HEAD.
- The webhook **waits** for the app's deploy lock when a manual rebuild or
  rollback holds it — it never steals or bypasses the lock. The lock itself is
  acquired and released by the rebuild pipeline, which stays authoritative.
- On shutdown, the job in flight is given up to 60s to finish; a rebuild
  subprocess that is already running is left to complete on its own (it is an
  independent CLI invocation — its isolated source is then removed by the next
  startup's sweep, never underneath the running build). Queued-but-unstarted jobs
  are dropped and logged.

## Exact-commit source preparation

A webhook-enabled app must live inside a Git repository with a configured remote
(usually `origin`); the repository is resolved from the app's existing source
directory — webhook payloads never name repository paths. Before each rebuild the
queued job:

1. validates the pushed commit SHA (hex, 40 or 64 chars) — payload values are
   never executed or interpolated into shell commands; Git always runs with
   structured arguments;
2. runs `git fetch <remote> <branch>` in the app's repository (a fetch, never a
   pull — the working tree never moves) and verifies the exact commit is now
   present (retrying with a direct SHA fetch for servers that allow it);
3. checks the commit out into a fresh **detached Git worktree** under
   `~/.phelix/webhook/worktrees/` — an isolated, private source for this one job.
   The application's working tree, branch, HEAD and uncommitted changes are never
   touched, and two queued commits never share source state. For an app
   configured in a subdirectory of a monorepo, the build uses the same
   subdirectory inside the worktree;
4. verifies the worktree HEAD equals the requested SHA, then runs `phelix rebuild
   <app> --source-dir <worktree>`.

The worktree is removed after the job — whether the build, deploy, health check
or rollback succeeded or failed (a cleanup failure is logged and never masks the
deployment's own outcome). Worktrees abandoned by a crashed daemon are swept at
the next startup (after a one-hour grace period so an orphaned rebuild can
finish).

If any Git step fails — not a repository, missing remote, fetch failure, commit
not found, worktree creation — the job fails **before** any rebuild runs and is
reported through the existing webhook event/log mechanism. A Git synchronization
failure never causes Phelix to build whatever is currently on disk.

Version metadata comes from the source that was actually compiled (the worktree's
HEAD, i.e. the exact pushed commit), so `versions.json` never claims a commit that
was not built.

## Deployment jobs, status & history

Every accepted delivery becomes a **durable deployment job** — persisted to disk
(`~/.phelix/webhook/jobs/`, one atomic JSON record per job) *before* the webhook
is acknowledged, so a delivery is never accepted without a record. A job carries
the delivery id, app, branch, the exact pushed commit, its lifecycle status and
stage, timestamps, the resulting Phelix version, and a classified error
code/message. It never stores secrets, request bodies or temporary worktree
paths.

Lifecycle: `accepted → queued → syncing → building → deploying → health_checking
→ succeeded` — or a terminal `failed` / `rolled_back` / `cancelled` from any
running state. The stage additionally tracks `queue`, `git_sync`, `build`,
`deploy`, `health_check`, `cleanup` and `recovery`. Failures are classified from
the rebuild pipeline's structured signals (its rendered error code and documented
exit codes): `GIT_SYNC_FAILED`, `BUILD_FAILED`, `DEPLOY_FAILED`,
`HEALTH_CHECK_FAILED`, `AUTO_ROLLBACK_FAILED`, `WEBHOOK_QUEUE_FAILED`. A
deployment that failed and was automatically rolled back is recorded as
`rolled_back` (detected from the rollback history the pipeline itself writes),
with the restored version.

On success the job records the Phelix version the pipeline created, matched by the
exact pushed commit — giving the chain delivery → job → commit → version →
outcome.

Query it locally (offline, no authentication, read-only):

```bash
phelix webhook status api            # in-flight jobs (live stages)
phelix webhook history api           # recent finished jobs
phelix webhook history api --limit 50
phelix webhook status api --json     # machine-readable records
```

```text
Webhook Deployments: api

JOB                 STATUS      STAGE         COMMIT   VERSION
wh_3a15c3383419c74e syncing      git_sync      93d8c58  -
```

```text
TIME             STATUS     COMMIT   VERSION   MESSAGE
2026-09-15 20:20 succeeded   93d8c58  v3       deployed
2026-09-15 20:20 failed      93d8c58  -        daemon restart interrupted the job while syncing; …
2026-09-15 20:18 succeeded   4621b9d  v2       deployed
```

## Restart recovery

If the daemon crashes or is killed, jobs left in a running state are closed out at
the next startup — **nothing is ever re-run** (the delivery ledger keeps the
replay guarantee, so a restart can never deploy the same delivery twice). For a
job whose rebuild subprocess had already started, the existing deployment state
decides: when the app's current version is the job's exact pushed commit and was
promoted after the job was accepted, the deployment demonstrably finished and the
job is marked `succeeded` with that version. Anything else is marked `failed` with
`error_code=WEBHOOK_JOB_INTERRUPTED` (never an invented success). A job dropped
while merely queued is `cancelled`. Redelivering the webhook (GitHub retries, or a
new delivery) deploys the commit again through the normal path.

Job history retention is bounded (the 200 most recent finished jobs; active jobs
are never deleted). This is independent from the delivery ledger's dedup window
(512 entries), so history trimming can never weaken replay protection.

## What it does not do

There is no build, deployment, health, version or rollback logic in the webhook
layer — everything runs through the existing `phelix rebuild` pipeline. Git
fetching uses the repository's own configured remote and credentials; the webhook
does not manage or store any Git credentials itself.

## Events

Webhook activity is reported through the existing event channel (subject to
[per-app watching](monitoring.md#per-app-watching-watching)) as application
events: `webhook_server_start`, `webhook_accepted`, `webhook_branch_mismatch`,
`webhook_duplicate`, `webhook_queue_failure`, `webhook_rebuild_failed` (and
`webhook_rejected` for authenticated-but-unreadable requests), plus the
durable-job lifecycle events `webhook_job_started`, `webhook_job_succeeded`,
`webhook_job_failed`, `webhook_job_rolled_back` and `webhook_job_recovered`, which
carry the correlation fields (job id, delivery id, branch, commit, stage, version)
in the event message. Signature rejections are logged locally only —
unauthenticated traffic must not be able to make the server emit backend traffic.
Local logs always carry the useful identifiers (app, job id, delivery ID, branch,
commit).

## Related

- [Configuration](../reference/configuration.md) — the `webhook` block.
- [Rollback](rollback.md) — the `--auto-rollback` behavior a job records as
  `rolled_back`.
- [Remote webhook management contract](../architecture/backend-contracts/remote-webhook-backend-contract.md)
  — dashboard-issued webhook management.
- [Security](security.md) — HMAC authentication, secret handling, `127.0.0.1`
  binding.
- [Command reference](../reference/commands.md#other-commands).
