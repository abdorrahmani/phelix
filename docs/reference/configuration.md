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
| `port` | no | Public application port. Default `8080` when missing. |
| `watching` | no | Backend monitoring opt-in: `enable` or `disable` (`phelix init` writes `disable`). Applied to the app on every build/rebuild; absent leaves the app's current state (see [Monitoring](../guides/monitoring.md)). |
| `health.endpoints[].name` | yes (per endpoint) | Endpoint name, e.g. `default`, `readiness`. Must be unique. |
| `health.endpoints[].path` | yes (per endpoint) | HTTP path on localhost, e.g. `/health`. Must start with `/`. |
| `health.endpoints[].interval` | no | Monitoring check interval (e.g. `10s`, `1m`). Default `10s`. |
| `health.endpoints[].retries` | no | Consecutive failures before marking DOWN. Default `3`. |
| `health.endpoints[].mode` | no | Deploy health tier: `auto` (default), `http`, `tcp-only`, `none`. |
| `deploy.strategy` | no | `classic` (default), `blue-green`, `rolling`, `canary`, or `progressive`. |
| `deploy.replicas` | no | Replica count for `rolling` (requires `deploy.strategy: rolling`). |
| `deploy.runtime` | no | `native` (default) or `docker` — run instances as containers. `docker` requires a zero-downtime strategy (see [Docker runtime](../guides/docker-runtime.md)). |
| `deploy.rollout.canary` | no | Traffic share (percent) for `canary` before promotion. Default `10`. |
| `deploy.rollout.duration` | no | Verification window for `canary` (e.g. `2m`). Default `30s`. |
| `deploy.rollout.steps[]` | no | Progressive steps: `traffic` (percent) and optional `duration` per step; must strictly increase and end at `100`. |
| `deploy.rollout.verification` | no | Regression thresholds: `interval`, `max_error_rate`, `max_error_delta`, `max_p95_factor` (see [Canary & progressive rollouts](../guides/canary-and-progressive-rollouts.md)). |
| `deploy.autoscaling` | no | Replica autoscaling block. **Parsed, validated, and resolved, but not yet executed** — see note below. |
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

## Autoscaling block (parsed but not yet executed)

The `deploy.autoscaling` block is accepted, validated, and resolved by the project
config layer (`internal/project/schema.go`), but **no component executes scaling
decisions yet** (the decision engine is observe-only). It is documented here for
completeness; do not expect it to change replica counts. See
[deployment architecture](../architecture/deployment.md#autoscaling-decision-engine-current-status).

```yaml
deploy:
  autoscaling:
    enabled: true
    min_replicas: 1
    max_replicas: 5          # defaults to deploy.replicas when unset
    interval: 15s            # default 15s
    cooldown: 60s            # default 60s
    evaluation_windows: 1    # consecutive samples a condition must hold; default 1
    cpu:
      scale_up: 70           # percent; default 70
      scale_down: 30         # percent; default 30
    latency:
      scale_up_p95: 500ms    # default 500ms
      scale_down_p95: 150ms  # default 150ms
```

> **Source vs README discrepancy (recorded for the report):** the `deploy.runtime`,
> `resources`, `matrix`, and `deploy.autoscaling` blocks all exist in the code and
> are validated on load. The README documented `resources`, `matrix`, and
> `deploy.runtime` in prose but **not** `deploy.autoscaling` (it was code-only).
> The autoscaling fields above are transcribed from the schema and its defaults
> (`internal/project/schema.go`, `internal/deploy/autoscale.go`) — the
> implementation is the source of truth here.

## Validation

The configuration is validated before it can affect a build or deploy — an invalid
file fails the command with a `CONFIGURATION_ERROR` instead of being ignored:

```text
Configuration error: invalid deploy.strategy "foobar"
Hint: expected one of: classic, blue-green, rolling, canary, progressive
```

Validated: YAML syntax, port range, strategy name, `replicas` (≥ 1, rolling only),
endpoint names (required, unique), paths (must start with `/`), durations (`10s`,
`1m`, …), modes (`auto`, `http`, `tcp-only`, `none`), and the rollout plan:
traffic percentages (whole numbers, `25` or `"25%"`), each step within `1-100`,
strictly increasing shares ending at the `100%` promotion, non-negative durations,
`canary` within `1-99`, and verification thresholds (positive interval, error
rates within `0-100`, p95 factor ≥ 1).

## Related

- [Command reference](commands.md) — how commands resolve these values.
- [Environment reference](environment.md) — env-var overrides (`PHELIX_*`).
- Guides: [Zero-downtime](../guides/zero-downtime-deployments.md),
  [Canary/progressive](../guides/canary-and-progressive-rollouts.md),
  [Health checks](../guides/health-checks.md), [Webhooks](../guides/webhooks.md),
  [Resource limits](../guides/resource-limits.md),
  [Matrix builds](../guides/matrix-builds.md).
