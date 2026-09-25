# Troubleshooting

A central entry point for diagnosing common Phelix problems. Each section links
to the reference or guide that covers the topic in depth rather than repeating
it. Where the available documentation does not define a recovery procedure, that
is stated explicitly instead of guessing.

## Before debugging

- **Read the error line.** Phelix renders one error on stderr with a short
  **code**, a message, and (for recognized problems) a suggested fix — see
  [error codes](../reference/error-codes.md).
- **Check the exit code.** It tells you the category of failure — see
  [exit codes](../reference/exit-codes.md).
- **Re-run with `--debug`** to see the full wrapped root-cause chain (secrets are
  still redacted).
- **Run `phelix doctor`** in the project directory — it checks project detection,
  toolchain, build tools, `phelix.yaml`, and whether the app honors the
  [PORT contract](../getting-started/the-port-contract.md).
- **Check status and logs:** `phelix status <app>`, `phelix log <app>`.

## Installation problems

- Verify the install with `phelix version`; on Linux check the monitor service
  with `sudo systemctl status phelix`.
- The installer downloads a prebuilt binary for your OS/arch — no Go/Rust
  toolchain is needed just to install. See
  [Installation](../getting-started/installation.md).
- **Unsupported platform:** Phelix targets Linux, with experimental Windows
  support. The available documentation does not define a supported install path
  beyond these; the one-line installer's systemd step is Linux-only and is a
  no-op elsewhere.

## Configuration problems

- **Symptom:** a command fails with `CONFIGURATION_ERROR` (exit code **40**).
- **Cause:** invalid `phelix.yaml` — bad YAML, unknown `deploy.strategy`,
  a health path not starting with `/`, an invalid rollout plan, etc.
- **What to check:** the hint printed with the error names the field; the full
  validation rules are in the [configuration reference](../reference/configuration.md#validation).
- **Relevant command:** `phelix doctor` reports whether `phelix.yaml` is found
  and valid.

## PORT and application startup problems

- **Symptom:** "port-validation failure" — the process runs but nothing listens
  on the requested port.
- **Cause:** the app hardcodes a port instead of reading `PORT`.
- **What to do:** make the app bind the port from `PORT`; see
  [The PORT Contract](../getting-started/the-port-contract.md) and the
  [simple-go](../../examples/simple-go/) / [simple-rust](../../examples/simple-rust/)
  examples.
- **Relevant command:** `phelix doctor` flags a hardcoded port (a Rust app may
  report the PORT check as an inconclusive ⚠ warning rather than a failure).
- **Port already in use** (`PORT_UNAVAILABLE`, exit **30**): the error names the
  listening process/PID when the OS can tell, and suggests `--port`. Phelix
  **never** stops the process for you.

## Build failures

- **Symptom:** `BUILD_FAILED` / `BUILD_TIMEOUT` / `TOOLCHAIN_NOT_FOUND` /
  `UNSUPPORTED_PROJECT` (exit **20**).
- **What to check:** the wrapped root cause (the failing `go`/`cargo` command and
  a bounded, redacted tail of its output) is shown on stderr without `--debug`.
- **Recognized cases with guidance** (see [error codes](../reference/error-codes.md)):
  missing Go/Rust toolchain (install command + docs link), and `go.mod` problems
  (`go mod init` / `go mod tidy` / cache-clear, each only for the matching
  condition).
- Unrecognized compiler failures keep their raw output; no fix is invented.

## Health-check failures

- **Symptom:** a deploy aborts because the new instance never became healthy;
  `HEALTH_CHECK_FAILED` (exit **21**).
- **Cause / what to check:** the deploy health tier gates traffic. Tier 1
  requires HTTP **2xx** on your configured `--path`; Tier 2 accepts any HTTP
  response; Tier 3 checks TCP or PID. A Tier-1 app that returns non-2xx on its
  health path will fail the gate. See [Health checks](health-checks.md).
- **What to do:** confirm the endpoint returns 2xx (`phelix health status <app>`,
  or `--watch` for a live view); prefer a real health endpoint so deploys use
  Tier 1.

## Deployment failures

- **Guarantee first:** a failed candidate is killed and the **currently active
  instance is left untouched** — a failed deploy never replaces the live version.
  See [Zero-downtime deployments](zero-downtime-deployments.md).
- **`DEPLOY_LOCKED` (exit 21):** another deploy/rollback holds the app's deploy
  lock. The lock is an OS file lock released automatically on process death; wait
  for the other operation, or if you are certain none is running, release it with
  `phelix deploy unlock <app>` (see [commands](../reference/commands.md#zero-downtime-reverse-proxy)).
- **`--auto-rollback`:** add it to `phelix rebuild` to restore the previous
  known-good version automatically on a deploy-phase failure. A degraded state
  where recovery itself fails exits **24** (`AUTO_ROLLBACK_FAILED`) — this
  requires manual intervention; see [Rollback](rollback.md).
- **Interrupted / crashed deploy:** blue-green slots left by a crashed deploy are
  reclaimed on the next run; a rollout interrupted mid-flight leaves the stable
  version serving and is restored on the next deploy.

## Rollback problems

- **`ROLLBACK_FAILED` (exit 22):** the rollback execution failed; the target
  version stays on disk and `is_current` is unchanged so you can retry.
- **`ROLLBACK_VERIFY_FAILED` (exit 23):** the rollback executed but did not stay
  healthy for the `--verify` window — execution and verification outcomes are
  tracked separately.
- **Preview first:** `phelix rollback <app> --to <v> --dry-run` shows the full
  plan with zero mutations.
- **No known-good version:** if no retained version's binary still exists, Phelix
  says so rather than fabricating a target. See [Rollback](rollback.md).

## Docker problems

- **Image build (`dockerize`):** requires Docker on the host; language is
  auto-detected from `go.mod`/`Cargo.toml` and fails clearly if neither or both
  are present. A user-provided `Dockerfile` is used as-is. `--push` needs
  `docker login` first. See [Docker image building](docker-images.md).
- **Docker runtime (`deploy.runtime: docker`):** requires a zero-downtime
  strategy (`classic` is native-only) — the config validator rejects
  `runtime: docker` with `strategy: classic`. Managed containers publish only to
  `127.0.0.1`; the proxy is the sole public entry point. See
  [Docker runtime](docker-runtime.md).
- `DOCKER_ERROR` is exit **50**.

## Matrix build problems

- **Partial failures are expected to surface:** the build phase is fail-open (one
  combination failing doesn't stop the rest) and the CLI exits non-zero if any
  combination failed. A run with failures never reports `succeeded`.
- **Push is fail-closed:** if any combination failed, nothing is pushed unless you
  pass `--push-partial`.
- **Inspect a run:** `phelix matrix show <run-id>` (per-combination status,
  attempts, SHA-256, redacted errors); `phelix matrix status` for the live run.
- **Recover:** automatic retries (`--matrix-retries`, transient failures only),
  `--resume` (incomplete combinations), or `phelix matrix retry <run-id> --failed`
  (a new linked run). See [Matrix builds](matrix-builds.md).

## Webhook problems

- **Responses are uniform JSON** and map to HTTP status: `401` invalid/missing
  signature, `404` unknown app or disabled webhook, `400` unreadable payload /
  missing delivery id, `413` body over 2 MiB, `503` queue unavailable (the
  provider should redeliver), `200` for accepted, branch-mismatch, ping, and
  duplicate. See the full table in [Webhooks](webhooks.md#endpoint).
- **Startup fails** if an app enables the webhook but its `secret_env` variable
  is not set.
- **Inspect jobs (offline, read-only):** `phelix webhook status <app>`,
  `phelix webhook history <app>`.
- **A Git sync failure never builds stale on-disk source** — the job fails before
  any rebuild.

## Authentication and TLS problems

- **Auth is optional:** build/run/deploy/rollback all work offline. Auth errors
  (exit **10**: `UNAUTHENTICATED` / `INVALID_CREDENTIALS` / `SESSION_EXPIRED`)
  prompt you to run `phelix auth login`.
- **Rate limits:** a throttled login reports when to retry (`Retry-After`); the
  monitor daemon backs off for the announced window. Retrying earlier only
  extends it. See [Authentication](authentication.md).
- **TLS:** the monitor gRPC channel uses TLS in every mode except `dev` (which
  uses plaintext for local backends). The available documentation does not define
  a certificate-repair procedure beyond the
  [TLS & certificate policy contract](../architecture/backend-contracts/tls-cert-policy.md).

## Monitoring problems

- **Nothing appears on the dashboard:** monitoring is per-app opt-in and
  **disabled by default**. Enable it with `phelix watch <app>` (or `watching:
  enable` in `phelix.yaml`). While no app is watched the monitor daemon does not
  even open the stream, and dashboard-issued remote commands cannot reach the
  server — enabling watching on any app re-opens it within seconds.
- **Requires an authenticated session.** See [Monitoring](monitoring.md).

## Resource and OOM problems

- **Symptom:** an instance is classified `RESOURCE_OOM`.
- **Cause:** the instance cgroup's `memory.events` `oom_kill` counter increased —
  it hit its configured `resources.memory` limit (distinct from a plain crash or
  `SIGKILL`). The message carries the configured limit and PID.
- **What to check:** the host requirements (cgroups v2, Linux 5.7+ with `clone3`,
  a delegated parent with the controllers enabled). A setup/attachment failure
  aborts launch rather than launching unlimited. See
  [Resource limits](resource-limits.md).
- Limits are Linux-only and apply to newly launched instances, not live
  processes.

## State and filesystem problems

- All state lives under `~/.phelix/` (relocatable with `PHELIX_DATA_DIR`); the
  full layout is in the [data-directory reference](../reference/data-directory.md).
- `master.key` (`0600`) encrypts env storage — **never delete it** if you have
  encrypted env you need; without it those values cannot be decrypted.
- Filesystem errors surface with their `os` root cause reachable via
  `errors.Is`. The available documentation does **not** define a manual
  state-repair or corruption-recovery procedure; prefer re-running the operation,
  and treat direct edits to files under `~/.phelix/` as unsupported.

## Error codes

Every rendered error carries a short code. The recognized-problem registry (with
concrete suggested fixes) and the general rendering rules are in
[error codes](../reference/error-codes.md).

## Exit codes

Exit codes are a stable, script-facing contract (same category ⇒ same code). The
full table is in [exit codes](../reference/exit-codes.md).

## Getting more information

- `--debug` on any command — full wrapped error chain on stderr (redacted).
- `phelix status <app>` / `phelix list` — runtime and deployment state.
- `phelix log <app>` — app logs; `phelix log` alone tails Phelix's own log.
- `phelix doctor` — project compatibility diagnosis.
- Read-only inspectors: `phelix build-report <app>`, `phelix rollback history
  <app>`, `phelix matrix show <run-id>`, `phelix webhook history <app>`.
