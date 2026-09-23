# Command Reference

Every command below also supports [interactive
auto-prompting](../guides/interactive-usage.md): run it without a required
argument and Phelix will ask for it (when stdin is a terminal).

## Building & rebuilding

### `phelix build <NAME> [flags]`

Compiles the project in the current directory (auto-detects Go or Rust), starts
it on the given port, and records a **new versioned build**. A **Build Report**
with regression analysis is printed automatically after every successful build
(see [Build reports](../guides/build-reports.md)).

`NAME` and `--port` are resolved in this order: CLI argument/flag →
[`phelix.yaml`](configuration.md) → default `8080` → interactive prompt. When the
config file supplies both, the command runs without asking anything.

| Flag | Default | Description |
|------|---------|-------------|
| `--port, -p` | `8080` | Port to run the app on (not required for matrix builds — they never start an instance) |
| `--build-arg, -a` | — | Extra args passed to the build tool (repeatable); in matrix mode they are passed to every `go build` / `cross build` |
| `--tag` | — | Human label stored with the version (e.g. `"hotfix-auth"`) |
| `--no-upload` | `false` | Don't sync app info to the server |
| `--debug` | `false` | Verbose build/tool output |
| `--matrix` | `false` | Matrix mode: build versions × platforms (see [Matrix builds](../guides/matrix-builds.md); also activates via the phelix.yaml matrix profile) |
| `--go-versions` | — | Go versions, e.g. `1.22,1.23,1.27` |
| `--rust-versions` | — | Rust versions, e.g. `1.77,1.78.2` |
| `--platforms` | — | Targets, e.g. `linux/amd64,linux/arm64` |
| `--matrix-concurrency` | 3 | Max parallel matrix builds |
| `--matrix-dry-run` | `false` | Print the plan without building |

```bash
phelix build myapp -p 8080 --tag "v1.2.3"
```

### `phelix rebuild <ID|AppName> [flags]`

Rebuilds an existing app from its source directory. Supports zero-downtime
deploy. Without an argument, the app is taken from `phelix.yaml` (`name:`) when it
exists in the current directory, otherwise an interactive picker is shown. The
deployment path is resolved in this order: explicit `--blue-green` / `--replicas`
→ `--strategy` → the config's [`deploy.strategy`](configuration.md) → the strategy
the app is currently deployed with → classic. An app already running blue-green or
rolling therefore stays on it unless a classic rebuild is asked for explicitly; a
rolling rebuild inherited this way keeps the replica count the app is running at.

| Flag | Default | Description |
|------|---------|-------------|
| `--port, -p` | previous/`8080` | Port to run on |
| `--build-arg, -a` | — | Extra build args (repeatable) |
| `--tag` | — | Version label |
| `--no-upload` | `false` | Skip server sync |
| `--strategy` | — | Strategy for this rebuild only: `classic`, `blue-green`, `rolling`, `canary`, or `progressive`. Overrides `phelix.yaml`; never written back to it |
| `--blue-green` | `false` | Zero-downtime blue-green deploy (needs `phelix proxy`) |
| `--replicas` | `0` | Zero-downtime rolling deploy over N replicas |
| `--canary` | `0` | Canary deploy: route N percent (1-99) of traffic to the new version, verify health and metrics, then promote. Verification window/thresholds come from `phelix.yaml` `deploy.rollout` (needs `phelix proxy` and an existing deployment) |
| `--auto-rollback` | `false` | On a deploy-phase failure (instance start, health check, traffic switch), automatically restore the previous known-good version. Build/compile failures never trigger it |
| `--source-dir` | app directory | Compile from this source directory instead of the app's directory (how the webhook deploys its isolated Git worktree). The app identity, output binary, ports and deployment state stay with the app; `phelix.yaml` and the recorded git commit come from the source directory |

```bash
phelix rebuild myapp --blue-green
phelix rebuild myapp --replicas 3
phelix rebuild myapp --canary 5
phelix rebuild myapp --strategy classic
phelix rebuild myapp --strategy rolling
```

`--strategy rolling` combined with `--replicas N` uses N replicas. `--canary`
cannot be combined with `--strategy`, `--blue-green` or `--replicas` — it already
names a strategy. An unrecognized value fails with `INVALID_ARGUMENT` (exit code
`2`) before anything is built. This is the same override the dashboard sends for a
remote rebuild (`canary`/`progressive` overrides resolve their plan from the app's
`phelix.yaml`). See
[Canary & progressive rollouts](../guides/canary-and-progressive-rollouts.md).

### `phelix init`

Detects the project type (Go/Rust) and creates `phelix.yaml` with the application
name, port, `watching: disable`, a default health endpoint, and the classic deploy
strategy (see [Configuration](configuration.md)). Prompts interactively in a TTY;
fully scriptable with flags:

```bash
phelix init --name api --port 3000 --yes
```

Never modifies application source code. If a hardcoded listen port is detected,
prints a warning and points to `phelix doctor`. It also accepts `--runtime docker`
to write a docker-runtime config (see [Docker runtime](../guides/docker-runtime.md)).

### `phelix doctor`

Diagnoses whether the current project is Phelix-compatible: project detection,
toolchain, build tools, `phelix.yaml`, and — most importantly — whether the
application listens on `PORT` instead of a hardcoded port.

```text
$ phelix doctor

Phelix Doctor

✓ Project detected        go (/home/me/api)
✓ go toolchain            go1.22 linux/amd64
✓ Build tools             ready
✓ phelix.yaml             found
✓ Application name        api
✓ Configured port         4000
✗ PORT configuration      hardcoded :3000

Summary:
  6 passed
  1 failed
```

Exits non-zero when a critical check fails. Inconclusive detection (no listener
found, no `PORT` usage) is reported as ⚠ warning, not a false failure. Diagnostic
only — never rewrites source code.

### `phelix build-report <AppName> [flags]`

Read-only inspection of stored build reports — never triggers a build, works fully
offline. See [Build reports](../guides/build-reports.md).

```bash
phelix build-report myapp            # 5 most recent reports
phelix build-report myapp --limit 10
```

## Versioning & rollback

Full behavior, flags, and examples are documented in [Rollback](../guides/rollback.md).

### `phelix rollback <AppName> [flags]`

Reverts to a previous versioned build. Automatically chooses the right strategy:
classic stop→start for plain builds, or zero-downtime for apps deployed with
blue-green/rolling.

| Flag | Default | Description |
|------|---------|-------------|
| `--to` | — | Target: `v3`, `3`, or a tag name (bypasses the interactive picker) |
| `--list` | `false` | List all retained versions with metadata (non-interactive) |
| `--dry-run` | `false` | Preview the rollback plan without changing application, process, proxy, or deployment state |
| `--reason` | — | Record why the rollback was performed (stored in rollback history; max 500 characters) |
| `--verify` | — | Observe rollback stability for a Go duration (e.g. `30s`, `1m`, `2m30s`) after the rollback completes |

### `phelix rollback history <AppName> [--limit N]`

Shows the recorded rollback outcomes for one application, newest first.

| Flag | Default | Description |
|------|---------|-------------|
| `--limit` | `20` | Maximum number of entries to show (must be a positive number) |

## Lifecycle

| Command | Description |
|---------|-------------|
| `phelix start [<ID\|AppName>] [--port P] [--ensure]` | Start an app (or all apps if none given). `--ensure` builds if missing and rebuilds if startup fails. |
| `phelix stop <ID\|AppName>` | Stop a running app. |
| `phelix restart <ID\|AppName>` | Restart an app. |
| `phelix status <ID\|AppName>` | Show status: PID, uptime, RAM/CPU, watching, version, deploy mode, proxy routing. |
| `phelix list` | Table of all apps with version, status, watching, deploy, and proxy columns. |
| `phelix log [<ID\|AppName>]` | Tail app logs (last 10 lines + live stream). No arg → Phelix's own logs. |
| `phelix remove <ID\|AppName>` | Stop and delete an app from management. |
| `phelix watch <ID\|AppName> [--disable]` | Enable (default) or disable backend monitoring for one app. See [Monitoring](../guides/monitoring.md). |

## Encrypted environment variables

`phelix env` manages per-app secrets encrypted at rest with **AES-256-GCM**. See
[Environment variables](../guides/environment-variables.md).

```bash
phelix env set    MyApp DATABASE_URL=postgresql://localhost/db
phelix env get    MyApp DATABASE_URL      # sensitive values are masked
phelix env list   MyApp                   # values shown as ***REDACTED***
phelix env unset  MyApp DATABASE_URL
phelix env check  MyApp DATABASE_URL      # exit-status friendly existence check
```

## Health checks

`phelix health` configures HTTP health endpoints and the deploy-time health tier.
See [Health checks](../guides/health-checks.md).

```bash
phelix health set <App> --path /health --interval 10s --retries 3 --mode auto
phelix health add  <App> --name "API" --url https://api.example.com/health
phelix health list   <App>
phelix health remove <App> --name "API"
phelix health status <App> [--watch]   # --watch = live terminal dashboard
```

### Health daemon (operator / internal control)

`phelix health daemon` starts the background health-check daemon that probes
configured endpoints. It is an operator/daemon-control command, not part of the
normal app workflow: on a standard systemd install the `phelix monitor` service
already drives this daemon, so you rarely invoke it directly.

```bash
phelix health daemon                # start in the background (CLI exits)
phelix health daemon --foreground   # run attached (for supervisors)
phelix health daemon status         # show whether the daemon is running (PID)
phelix health daemon stop           # stop the background daemon (graceful, ~5s)
```

Per-app health configuration and the deploy-time tiers are covered in
[Health checks](../guides/health-checks.md); the reconcile loop is described in
[monitoring architecture](../architecture/monitoring.md).

## Zero-downtime reverse proxy

The `phelix proxy` daemon binds each app's public port and routes traffic to the
active internal instance. See [Zero-downtime deployments](../guides/zero-downtime-deployments.md).

```bash
phelix proxy              # start in background (detaches and returns)
phelix proxy status       # show enrolled apps + routing
phelix proxy stop         # shut the daemon down (drains up to 30s)
phelix proxy --foreground   # run attached (for process supervisors / systemd)
```

`phelix deploy unlock <AppName>` releases a stale per-app deploy lock (the lock is
an OS file lock normally released automatically; see
[deployment architecture](../architecture/deployment.md)).

## Docker image building

`phelix dockerize <AppName>` builds optimized multi-stage Docker images for Go and
Rust projects. See [Docker image building](../guides/docker-images.md).

```bash
phelix dockerize myapp --tag v1.0.0
phelix dockerize myapp --tag v1.0.0 --push --registry ghcr.io/myuser
phelix dockerize myapp --tag v1.0.0 --with-compose --depends-on redis,postgres
```

| Flag | Description |
|------|-------------|
| `--tag` | Version tag for the Docker image (e.g. `v1.2.3`, default `latest`) |
| `--push` | Push the image to the registry after building |
| `--registry` | Registry prefix (e.g. `ghcr.io/user`, `docker.io/myorg`, `harbor.example.com/project`) |
| `-a, --build-arg` | Extra build argument `KEY=VALUE` (repeatable; malformed entries are rejected with `INVALID_ARGUMENT`) |
| `--with-compose` | Generate a `docker-compose.yml` with the app service |
| `--depends-on` | Sidecar services for compose (`redis`, `postgres`, `mysql`, `mongodb`, `rabbitmq`) |
| `--matrix` + family | Matrix Docker builds — see [Matrix builds](../guides/matrix-builds.md) |

## Matrix builds

Build the cross-product of toolchain version × platform in one command. Full flag
reference and run-management subcommands (`matrix list/show/status/retry/init`)
are in [Matrix builds](../guides/matrix-builds.md).

```bash
phelix build myapp --matrix --go-versions 1.22,1.23,1.27 --platforms linux/amd64,linux/arm64
phelix matrix list
phelix matrix show mx_20260909_8f31
phelix matrix status
phelix matrix retry mx_20260909_8f31 --failed
phelix matrix init
```

## Other commands

```bash
phelix version [--short | --verbose]
phelix update [--check]   # update the Phelix binary to the latest release
phelix monitor      # start the long-running gRPC monitoring daemon (foreground)
phelix webhook      # start the Git push webhook server (foreground; see Webhooks)
phelix webhook status <app>   # active webhook deployment jobs (offline, read-only)
phelix webhook history <app>  # recent webhook deployment history (offline, read-only)
phelix wizard       # guided interactive menu (see Interactive usage)
```

- `phelix version` — see [installation](../getting-started/installation.md).
- `phelix update` — see [installation](../getting-started/installation.md#updating);
  internals in [release process](../development/release-process.md).
- `phelix monitor` — see [Monitoring](../guides/monitoring.md).
- `phelix webhook` — see [Webhooks](../guides/webhooks.md).
- `phelix wizard` — see [Interactive usage](../guides/interactive-usage.md).

## Related

- [Configuration](configuration.md) — the `phelix.yaml` values these commands
  resolve.
- [Exit codes](exit-codes.md), [Error codes](error-codes.md).
