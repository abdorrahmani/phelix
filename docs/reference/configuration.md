# Configuration Reference (`phelix.yaml`)

`phelix.yaml` is the project-level Phelix configuration file, stored in the
current project directory. It removes the need to repeat the application name,
port, health checks, and deployment strategy on every command.

The configuration file is fully optional. Projects without `phelix.yaml` keep
working exactly as before — every existing flag and prompt is unchanged.

## Creating the configuration

```bash
phelix init
```

detects the project type (Go/Rust) and generates:

```yaml
# Phelix project configuration.
# CLI flags override these values (e.g. --port).
name: my-app
port: 8080
watching: disable
health:
    endpoints:
        - name: default
          path: /health
          interval: 10s
          retries: 3
          mode: auto
deploy:
    strategy: classic
```

## Full example

```yaml
name: api
port: 3000
watching: enable

health:
  endpoints:
    - name: readiness
      path: /ready
      interval: 5s
      retries: 3
      mode: http

    - name: liveness
      path: /health
      interval: 10s
      retries: 3
      mode: auto

deploy:
  strategy: blue-green

webhook:
  enabled: true
  branch: main
  secret_env: PHELIX_WEBHOOK_SECRET
```

## Field reference

| Field | Required | Description |
|-------|----------|-------------|
| `name` | no* | Application name used by `build`, `rebuild`, `health`, `rollback`, etc. *Optional: when missing, `build` prompts for it (TTY) or fails with a clear error (scripts). |
| `type` | no | Workload type: `service` (the default; every file written before the key existed), `worker`, a supervised background process with no public port (see [Workers](../guides/workers.md)), or `job`, a one-shot command run on a cron schedule (see [Jobs](../guides/jobs.md)). |
| `port` | no | Public application port. Default `8080` when missing. Not valid for `type: worker` or `type: job`. |
| `watching` | no | Backend monitoring opt-in: `enable` or `disable` (`phelix init` writes `disable`). Applied to the app on every build/rebuild; absent leaves the app's current state (see [Monitoring](../guides/monitoring.md)). |
| `health.endpoints[].name` | yes (per endpoint) | Endpoint name, e.g. `default`, `readiness`. Must be unique. |
| `health.endpoints[].path` | yes (per endpoint) | HTTP path on localhost, e.g. `/health`. Must start with `/`. |
| `health.endpoints[].interval` | no | Monitoring check interval (e.g. `10s`, `1m`). Default `10s`. |
| `health.endpoints[].retries` | no | Consecutive failures before marking DOWN. Default `3`. |
| `health.endpoints[].mode` | no | Deploy health tier: `auto` (default), `http`, `tcp-only`, `none`. |
| `health.endpoints[].codes` | no | Expected HTTP status codes for the endpoint check: a range (`200-299`) or a list (`200,204`). Default `200-299`. |
| `health.endpoints[].timeout` | no | Per-request timeout (e.g. `5s`). Default `10s`. |
| `health.replace` | no | Default `false`: the block merges with endpoints configured by `phelix health add/set`. `true` drops the endpoints the file does not declare (their names are printed). |
| `deploy.strategy` | no | `classic` (default), `blue-green`, `rolling`, `canary`, or `progressive`. Workers support `classic` and `rolling` only. |
| `deploy.replicas` | no | Replica count for `rolling` (requires `deploy.strategy: rolling`). Workers may declare it under `classic` too. |
| `deploy.runtime` | no | `native` (default) or `docker` — run instances as containers. `docker` requires a zero-downtime strategy (see [Docker runtime](../guides/docker-runtime.md)). |
| `deploy.rollout.canary` | no | Traffic share (percent) for `canary` before promotion. Default `10`. |
| `deploy.rollout.duration` | no | Verification window for `canary` (e.g. `2m`). Default `30s`. |
| `deploy.rollout.steps[]` | no | Progressive steps: `traffic` (percent) and optional `duration` per step; must strictly increase and end at `100`. |
| `deploy.rollout.verification` | no | Regression thresholds: `interval`, `max_error_rate`, `max_error_delta`, `max_p95_factor` (see [Canary & progressive rollouts](../guides/canary-and-progressive-rollouts.md)). |
| `schedule.cron` | yes (job) | Cron expression: standard five fields (`minute hour day month weekday`) or `@hourly`/`@daily`/`@weekly`/`@monthly`/`@yearly`. Only valid with `type: job`. |
| `schedule.timezone` | no (job) | IANA timezone the expression is evaluated in. Default `UTC`. The expression must not embed `TZ=` or `CRON_TZ=`. |
| `schedule.missed` | no (job) | What happens to occurrences missed while the monitor was down. Only `skip` is supported (the default): missed occurrences never run and are not replayed. |
| `job.command` | yes (job) | Command argv run without a shell, from the project directory. Relative paths fall back to the current built version binary when they no longer exist after a build. |
| `job.timeout` | no (job) | Time limit for one attempt, as a Go duration (e.g. `2m`). Default `30m`. On expiry the whole process group is killed. |
| `job.retries` | no (job) | Extra attempts after a failed attempt. Default `0`. Attempts run sequentially, never in parallel. |
| `job.concurrency.policy` | no (job) | `forbid` (default) or `allow`. `forbid` serializes runs of the same job across processes; `allow` permits overlap. |
| `deploy.rolling` | no | Worker rolling-replacement parameters. Only valid with `type: worker`. |
| `deploy.rolling.max_surge` | no | Replacements started above the desired count. Only `1` is supported (sequential replacement). Default `1`. |
| `deploy.rolling.max_unavailable` | no | Replicas that may be down during replacement. Only `0` is supported. Default `0`. |
| `deploy.rolling.drain_timeout` | no | Wait after SIGTERM when retiring a replaced replica (e.g. `60s`). Falls back to `process.shutdown.grace_period`. |
| `process.restart.policy` | no (worker) | Worker crash policy: `always` (restart on any exit), `on-failure` (default — restart on non-zero exit, signal or OOM) or `never`. Only valid with `type: worker`. |
| `process.restart.max_attempts` | no (worker) | Consecutive failure-driven restarts before the replica enters `crashloop`. Default `10`. |
| `process.restart.backoff.initial` | no (worker) | First restart delay (e.g. `2s`). Default `2s`. |
| `process.restart.backoff.max` | no (worker) | Restart delay cap (e.g. `60s`). Default `60s`. |
| `process.restart.backoff.multiplier` | no (worker) | Growth factor per consecutive attempt. Default `2`. |
| `process.shutdown.signal` | no (worker) | Signal sent on stop: `SIGTERM` (default) or `SIGINT`. |
| `process.shutdown.grace_period` | no (worker) | Wait between the signal and SIGKILL (e.g. `30s`). Default `30s`. |
| `process.startup_timeout` | no (worker) | Window a fresh replica has to come up: alive plus, in exec mode, a passing probe. Default `30s`. |
| `health.mode` | no (worker) | Worker health mode: `process` (default, identity liveness) or `exec`. Only valid with `type: worker`; services use `health.endpoints` instead. |
| `health.exec.command` | yes (with `mode: exec`) | Probe argv, run without a shell from the app directory with the replica's environment. Exit 0 means healthy. |
| `health.exec.interval` | no (worker) | Probe cadence (e.g. `10s`). Default `10s`. |
| `health.exec.timeout` | no (worker) | Per-probe timeout (e.g. `5s`); enforced with a process-group kill. Default `5s`. |
| `health.exec.retries` | no (worker) | Consecutive probe failures that cause ONE health-driven restart. Default `3`. |
| `deploy.autoscaling` | no | Replica autoscaling block for rolling services and workers. The monitor controller evaluates metrics and scales via the existing scaling paths (see [Autoscaling block](#autoscaling-block)). |
| `webhook.enabled` | no | Turns the Git push webhook trigger on for this app. Default `false`; apps without a `webhook` section never accept deliveries (see [Webhooks](../guides/webhooks.md)). |
| `webhook.branch` | yes (with `enabled`) | Bare branch name whose pushes trigger a rebuild (e.g. `main`, `release/2.x` — not `refs/heads/main`). |
| `webhook.secret_env` | yes (with `enabled`) | Name of the environment variable holding the HMAC-SHA256 shared secret (e.g. `PHELIX_WEBHOOK_SECRET`). The secret itself is never stored in phelix.yaml. |
| `resources.cpu` | no | Per-instance CPU ceiling, millicores (`500m`) or cores (`0.5`), Linux only (see [Resource limits](../guides/resource-limits.md)). |
| `resources.memory` | no | Per-instance memory limit with binary units (`512Mi`, `1Gi`), Linux only. |
| `matrix` | no | Build-matrix profile (see below and [Matrix builds](../guides/matrix-builds.md)). |

## Resolution order

`phelix build` (and the other project-aware commands) resolve values in this
order:

```text
CLI argument/flag  →  phelix.yaml  →  Phelix default  →  interactive prompt
```

Partial configurations work: a file with only `name:` prompts for the port (in a
TTY) or uses the default `8080`; a file with only `port:` prompts for the name. No
file at all means the prompt-or-explicit-args behavior. Explicit flags always win:

```bash
phelix build --port 8080        # uses port 8080 even though the config says 3000
phelix build my-custom-name     # uses my-custom-name even though the config has a name
```

## Deployment strategies

`deploy.strategy` selects which existing deployment path `phelix rebuild` uses —
it does not introduce a new deployment system.

```yaml
deploy:
  strategy: classic          # normal stop → rebuild → start (the default)
```
```yaml
deploy:
  strategy: blue-green       # zero-downtime blue-green (same as --blue-green)
```
```yaml
deploy:
  strategy: rolling          # zero-downtime rolling (same as --replicas N)
  replicas: 3
```
```yaml
deploy:
  strategy: canary           # one-shot canary share, then promotion
  rollout:
    canary: 5%               # traffic share for the canary (default 10%)
    duration: 2m             # verification window before promotion (default 30s)
```
```yaml
deploy:
  strategy: progressive      # step-by-step traffic increase
  rollout:
    steps:
      - traffic: 5%
        duration: 2m
      - traffic: 25%
        duration: 5m
      - traffic: 50%
        duration: 5m
      - traffic: 100%        # the final promotion step (required)
    verification:
      interval: 5s           # health/metrics poll cadence (default 5s)
      max_error_rate: 5      # absolute canary error-rate cap, percent (default 5)
      max_error_delta: 2     # canary minus baseline, percentage points (default 2)
      max_p95_factor: 3      # canary p95 at most N × baseline p95 (default 3)
```

Explicit flags still override the config (`--blue-green`, `--replicas`,
`--canary`, `--port`), and `--strategy` overrides it for a single rebuild without
editing the file. When nothing names a strategy, a rebuild keeps the one the app
is already deployed with (from `deploy.json`) instead of falling back to classic:
demoting a live blue-green/rolling app tears its instances down, so it has to be
asked for — `strategy: classic` here, or `--strategy classic` for one rebuild. A
rollout runs on the blue-green topology, so an app whose last deploy was
canary/progressive inherits blue-green when nothing names a strategy; put
`strategy: canary` or `progressive` in `phelix.yaml` to make rollouts the app's
default. `phelix rollback` needs no configuration: it inspects the recorded deploy
state and automatically uses classic or zero-downtime rollback to match how the
app was actually deployed. `phelix proxy` is required for
blue-green/rolling/canary.

## Worker workloads (`type: worker`)

A worker is a long-running background process (a queue consumer, an email
sender) with no public port: Phelix never allocates a port, never injects
`PORT`, never creates proxy routes, and judges startup by process identity and
the health policy instead. Services need no `type` key; the worker keys above
are rejected for services, and service keys (`port`, `health.endpoints`,
`deploy.rollout`, `deploy.autoscaling`, `deploy.runtime: docker`) are rejected
for workers.

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
    signal: SIGTERM
    grace_period: 30s
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
    drain_timeout: 60s

resources:                    # per replica, cgroups v2 on Linux
  cpu: 500m
  memory: 512Mi
```

Restart delays grow `initial * multiplier^attempt`, capped at `max`, with
jitter; after `max_attempts` consecutive failures the replica enters
`crashloop` until a manual restart or redeploy. A clean exit (code 0) is never
a failure: under `on-failure` the replica is marked `completed`. Rolling
replacement is one replica at a time; on a failed deploy the replicas that
were already replaced are reverted automatically. Full semantics, day-2
commands and the recovery contract are in
[Workers](../guides/workers.md); a runnable example lives in
[`examples/worker-go`](../../examples/worker-go/).

## Job workloads (`type: job`)

A job is a one-shot command, not a service and not a supervised process. Phelix
builds it as a version, then runs it on a cron schedule from the monitor. A run
that exits 0 is `succeeded`; a non-zero exit is `failed`. There is no port, no
proxy route, no health endpoint, no deploy strategy and no restart supervision:
the exit status is the result. Service and worker keys are rejected for jobs,
and `schedule`/`job` are rejected for services and workers.

```yaml
name: nightly-report
type: job

schedule:
  cron: "0 3 * * *"             # every day at 03:00
  timezone: Europe/Paris        # optional, default UTC
  missed: skip                  # the only supported policy in v1

job:
  command: ["./nightly-report", "-message", "nightly report"]
  timeout: 2m                   # per attempt, default 30m
  retries: 1                    # extra sequential attempts, default 0
  concurrency:
    policy: forbid              # forbid (default) or allow
```

`$HOME/.phelix/jobs/<name>/executions/<id>.json` stores every execution and
`logs/<id>.log` keeps its output (secrets redacted). The 200 most recent
finished executions are retained; running or pending records are never
pruned. `phelix job list`, `run`, `history` and `logs` inspect and trigger jobs
by hand. Full scheduling, recovery and retention behavior is in
[Jobs](../guides/jobs.md); a runnable example lives in
[`examples/job-go`](../../examples/job-go/).

## Per-instance CPU and memory limits (Linux)

```yaml
resources:
  cpu: "500m"
  memory: "512Mi"
```

Both fields optional; Linux/cgroups-v2 only. Full semantics, host requirements,
and OOM behavior are in [Resource limits](../guides/resource-limits.md).

## Matrix profile

```yaml
matrix:
  enabled: true
  go:                # or rust: — exactly one ecosystem
    versions: ["1.25", "1.26", "1.27"]
  platforms: [linux/amd64, linux/arm64, windows/amd64]
  concurrency: 4     # optional, default 3
  retries: 2         # optional
  include:           # optional, extra combinations / metadata
    - go: "1.28"
      platform: linux/amd64
      tag: latest
  exclude:           # optional, drop matching combinations
    - go: "1.25"
      platform: windows/amd64
```

Full expansion, precedence, and validation rules are in
[Matrix builds](../guides/matrix-builds.md).

## Autoscaling block

The `deploy.autoscaling` block configures automatic replica scaling. The
monitor daemon runs a per-app controller (`internal/autoscale`) that samples
per-replica CPU and memory plus windowed proxy traffic, feeds the decision
engine (`internal/deploy/autoscale.go`), and executes decisions through the
existing scaling paths: `deploy.WorkerScale` for worker apps and the rolling
scale executor for rolling services. Every replica mutation runs under the
same per-app deploy lock as deploys, rollbacks and manual scaling.

```yaml
deploy:
  autoscaling:
    enabled: true
    min_replicas: 1
    max_replicas: 5          # defaults to deploy.replicas when unset
    interval: 15s            # evaluation cadence; default 15s

    # Legacy top-level thresholds remain valid; the metrics block overrides
    # per metric when declared.
    cpu:
      scale_up: 70           # percent; default 70
      scale_down: 30         # percent; default 30
    latency:
      scale_up_p95: 500ms    # default 500ms
      scale_down_p95: 150ms  # default 150ms

    # Extended metrics block. A metric without a threshold for a direction
    # does not participate in that direction.
    metrics:
      cpu:
        scale_up: 65
        scale_down: 30
      memory:                  # percent of the app's resources.memory limit
        scale_up: 80
        scale_down: 40
      requests_per_second:     # proxy request rate
        scale_up: 500
        scale_down: 50
      latency:
        scale_up_p95: 200ms
        scale_down_p95: 80ms
        scale_up_p99: 500ms    # optional
        scale_down_p99: 200ms

    # Per-direction cadence. The top-level cooldown/evaluation_windows are
    # shorthand for both directions.
    scale_up:
      evaluation_windows: 3
      cooldown: 60s
    scale_down:
      evaluation_windows: 5
      cooldown: 180s
      stabilization_window: 300s  # the down condition must hold this long
```

Decision policy: scale up when ANY enabled metric breaches its upper
threshold; scale down only when EVERY enabled metric is at or below its lower
threshold; scale up wins conflicts. Missing metrics never trigger a scale-up
and block scale-down. Traffic metrics apply only when a proxy is enrolled.
Stale samples (older than twice the interval) produce no decision. Every
decision and outcome lands in the app's `autoscale_history.jsonl` and in an
`ops` record.

CPU is measured per logical core: a replica using two full cores reports 200
percent. Memory percent is relative to that replica's configured
`resources.memory` limit, not the host's total RAM. Without a configured
memory limit the memory percentage is unavailable and does not participate in
decisions; host-relative usage is reported separately in monitoring and status
output.

Manage it with `phelix autoscale status|enable|disable|pause|resume|evaluate|history`.
The runtime pause survives monitor restarts; enable/disable edits phelix.yaml.
Remote management goes through the `autoscale_*` MonitorStream commands,
gated by the `scale` authorization action.

Workers and rolling services both accept the block. See
[deployment architecture](../architecture/deployment.md#autoscaling-decision-engine-current-status).

## Lifecycle hooks

The optional `hooks` block runs commands at defined build, deploy, rollback and
stop points. Each hook takes an argument array, a timeout, retries and a failure
policy. Hooks without an explicit policy are critical except `post_deploy`,
`post_rollback` and `pre_stop`, whose failures are reported without undoing a
completed operation.

```yaml
hooks:
  pre_deploy:
    command: ["./bin/migrate", "up"]
    timeout: 5m
    retries: 2
  post_deploy:
    command: ["./scripts/notify"]
    continue_on_error: true
```

See [Deployment Lifecycle Hooks](../guides/lifecycle-hooks.md) for ordering,
environment variables, recovery and safe migration guidance.

## Validation

The configuration is validated before it can affect a build or deploy. An invalid
file fails the command with a `CONFIGURATION_ERROR` instead of being ignored:

```text
Configuration error: invalid deploy.strategy "foobar"
Hint: expected one of: classic, blue-green, rolling, canary, progressive
```

Validated: YAML syntax, the document count (one document per file), duplicate
mapping keys, port range, strategy name, `replicas` (≥ 1, rolling only, and
workers may also declare it under `classic`), endpoint names (required, unique), paths
(must start with `/`), durations (`10s`, `1m`, …), modes (`auto`, `http`,
`tcp-only`, `none`), and the rollout plan: traffic percentages (whole numbers,
`25` or `"25%"`), each step within `1-100`, strictly increasing shares ending at
the `100%` promotion, non-negative durations, `canary` within `1-99`, and
verification thresholds (positive interval, error rates within `0-100`, p95
factor ≥ 1).

For jobs additionally: the type value itself, a parseable cron expression, an IANA timezone, a positive `job.timeout`, `job.retries >= 0`, the concurrency policy vocabulary (`forbid`/`allow`), a non-empty `command`, and both `schedule` and `job` blocks. A job must not declare `port`, `health`, `process` or `deploy`; services and workers must not declare `schedule` or `job`.

For workers additionally: the type value itself, the restart policy vocabulary
(`always`/`on-failure`/`never`), backoff bounds (positive durations,
multiplier ≥ 1), the shutdown signal (`SIGTERM`/`SIGINT`), the health mode
(`process`/`exec`), `exec` requiring a non-empty `command`, the rolling bounds
(`max_surge: 1`, `max_unavailable: 0` in v1), and the cross-type exclusions in
both directions (a worker with a `port` or endpoint probes; a service with a
`process` block or `health.mode`). An invalid combination fails with a
`CONFIGURATION_ERROR` naming the exact yaml path.

### Checking the file without running a command

```bash
phelix config lint          # validate phelix.yaml in the current directory
phelix config lint --json   # the machine envelope, for scripts and agents
```

`lint` decodes the file with strict key checking. Unknown keys fail with the
dotted key path and source line, for example:

```text
Configuration error: /home/me/api/phelix.yaml has unknown key(s):
  /home/me/api/phelix.yaml:12: deploy.repilcas
Hint: remove or rename the key; see 'phelix config schema' for the accepted keys
```

Exit codes follow the standard contract: `0` when the file is valid, `40`
(`CONFIGURATION_ERROR`) when it is not, and `12` (`NOT_FOUND`) when there is no
file to check.

### Strict key checking transition

Strict checking starts in `phelix config lint`. Execution commands (`build`,
`rebuild`, `deploy`, `doctor` and the rest) still load a file with unknown keys
during this transition, but log a warning for each unknown key with the same key
path and source line. This keeps existing projects working while making typos
visible.

The next documented breaking release will make the shared execution loader
strict as well. Until then, `lint` and execution disagree only on unknown keys;
all other validation, including YAML syntax, duplicate keys, multiple documents
and contradictory workload settings, is identical.

## Editor support

`schema.json` in the repository root describes every key of `phelix.yaml`
(types, enums, ranges, required fields and the webhook dependency), generated
from the same structs the loader decodes into. Print it at any time with:

```bash
phelix config schema > schema.json
```

Point a YAML language server at it by adding a comment to the top of the file:

```yaml
# yaml-language-server: $schema=./schema.json
```

That gives completion and hover documentation in VS Code, Neovim, Helix and any
other editor using the YAML language server. The `$schema` comment can also
point at a checked-in copy elsewhere in the repository, or at a URL if you host
one.

## Related

- [Command reference](commands.md) — how commands resolve these values.
- [Environment reference](environment.md) — env-var overrides (`PHELIX_*`).
- Guides: [Zero-downtime](../guides/zero-downtime-deployments.md),
  [Canary/progressive](../guides/canary-and-progressive-rollouts.md),
  [Health checks](../guides/health-checks.md), [Webhooks](../guides/webhooks.md),
  [Resource limits](../guides/resource-limits.md),
  [Matrix builds](../guides/matrix-builds.md),
  [Workers](../guides/workers.md).
