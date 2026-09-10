# Phelix

A CLI tool for building, running, deploying, and managing **Go** and **Rust** applications — with versioned builds, zero-downtime blue-green/rolling deploys, Docker image building, encrypted environment variables, health checks, and multi-server monitoring.

Phelix is written in Go and built with [Cobra](https://github.com/spf13/cobra). It targets Linux (with experimental Windows support).

## Overview

Phelix helps you build, run, and manage Go and Rust applications across a single machine or multiple servers, with a comprehensive set of commands for authentication (optional), monitoring, and full application lifecycle management.

## Features

- Multi-server application management
- Real-time application monitoring (dashboard at `phelix.anophel.com`, opt-in via login)
- Centralized logging
- Cross-server status checking
- Optional authentication (build/run works offline; login enables dashboard sync)
- Application lifecycle management
- gRPC-based monitoring service (persistent, TLS-secured, auto-reconnecting)
- **Encrypted environment variable management** (AES-256-GCM)
- **Zero-downtime blue-green and rolling deploys** (via `phelix proxy`)
- **Canary & progressive rollouts** (route a percentage of traffic to the new version, verify health and metrics against the stable baseline, promote step by step — automatic rollback to the stable version on any regression)
- **Versioned builds with zero-downtime rollback** (all builds create versioned artifacts; `--tag` for meaningful labels)
- **Build Reports + Regression Alerts** (every successful build automatically records metrics — compiler, duration, cache status, binary size — and compares them against previous comparable builds to detect meaningful regressions, fully offline)
- **Docker image building** (auto-generated multi-stage Dockerfiles for Go/Rust with optimized layer caching)
- **Matrix builds** (build multiple compiler-version × platform combinations in one command)

---

## Table of Contents

- [Quick Start](#quick-start)
- [Installation](#installation)
- [Updating](#updating)
- [Uninstalling](#uninstalling)
- [Authentication (optional)](#authentication)
- [Interactive Wizard](#interactive-wizard)
- [Core Workflow](#core-workflow)
- [Project Configuration (phelix.yaml)](#project-configuration-phelixyaml)
- [Command Reference](#command-reference)
- [Run Phelix in Docker](#run-phelix-in-docker)
- [Multi-Server Monitoring](#multi-server-monitoring)
- [System Requirements](#system-requirements)
- [Where Phelix Stores Data](#where-phelix-stores-data)
- [Error Handling](#error-handling)
- [Best Practices](#best-practices)
- [Security Considerations](#security-considerations)
- [Support](#support)

## Quick Start

```bash
# Initialize project config (creates phelix.yaml)
phelix init

# Build and run an app from the current directory (auto-detects Go or Rust)
phelix build myapp --port 8080

# Rebuild after a code change — zero downtime via blue-green
phelix rebuild

# Ship a change to 5% of traffic first, verify, then promote
phelix rebuild --canary 5 myapp --blue-green

# Diagnose project compatibility (PORT usage, config, toolchain)
phelix doctor

# List all managed apps
phelix list

# Roll back to the previous versioned build
phelix rollback myapp
```

---

## Installation

### Recommended — one-line installer

```bash
curl -fsSL https://phelix.anophel.com/install.sh | bash
```

The installer **detects your operating system and architecture**, downloads the
matching prebuilt binary (so you don't need Go/Rust preinstalled just to install
Phelix), installs it to `/usr/local/bin/phelix`, and — on Linux — registers and
enables a `phelix.service` **systemd** unit that runs `phelix monitor` directly.
The monitor daemon runs in the foreground: it restores the managed apps that
were previously running and keeps a persistent TLS-secured gRPC connection to the
backend, reconnecting with exponential backoff.

```bash
phelix version              # verify the install
sudo systemctl status phelix   # Linux: monitor service running?
```

### Updating

```bash
phelix update            # download and install the latest release
phelix update --check    # only report whether an update is available
```

`phelix update` upgrades the Phelix CLI/agent binary itself to the latest
stable release, using the **same release server and layout as the one-line
installer** (`https://phelix.anophel.com/releases/<version>/phelix-<os>-<arch>`),
so no Go/Rust toolchain is needed. It:

- Resolves the latest stable version and compares it against the running one
  with proper semantic-version ordering (`v1.2.3` and `1.2.3` are the same
  version; a locally newer build is never downgraded).
- Downloads the matching prebuilt binary for the current platform into a
  temporary directory.
- Verifies the release's **SHA-256 checksum** when one is published — a
  mismatch aborts the update. When no checksum is published, it warns and
  continues (same policy as the installer).
- Validates the download is a genuine Phelix binary for this platform before
  touching anything, then **atomically replaces** the currently installed
  executable (the old binary is preserved until the new one is proven in
  place, and restored automatically if a later step fails).
- On Linux, if the `phelix.service` systemd unit is running, it **restarts
  the monitor service** afterwards and waits for it to become active again.
  `KillMode=process` means the restart stops only the monitor — managed
  applications keep running. A service that is installed but stopped is left
  stopped, and hosts without systemd (or macOS) simply get the binary
  replacement.
- Replacing a binary under `/usr/local/bin` needs root: `phelix update`
  detects this, authenticates `sudo` interactively when a terminal is
  attached, and fails with an actionable error otherwise. It never asks for
  root when the binary lives somewhere you can already write.

If Phelix is already up to date:

```text
✓ Phelix is already up to date (v1.2.3).
```

Application state under `~/.phelix/` (sessions, builds, environment
variables, version history) is **never modified** — the command only replaces
the binary itself and never rebuilds your applications.

### Uninstalling

```bash
curl -fsSL https://phelix.anophel.com/install.sh | bash -s -- --uninstall
```

The uninstaller (same one-line script, with `--uninstall`) reverses the
install step by step:

- **Stops and disables** the `phelix.service` systemd unit on Linux (the
  service is stopped, disabled, the unit file at
  `/etc/systemd/system/phelix.service` is removed, and `systemctl
  daemon-reload` is run). No-op where there is no systemd (e.g. macOS).
- **Removes the binary** from `/usr/local/bin/phelix` (or your `--install-dir`).
- **Removes the legacy** `phelix-startup.sh` wrapper left behind by older
  installs, if present.

```bash
sudo systemctl status phelix   # should be gone (Linux)
command -v phelix              # should print nothing
```

> **Note:** managed apps and all state under `~/.phelix/` are intentionally
> left in place, so you can reinstall later without losing your apps. Remove
> the data directory manually if you want a clean slate:
>
> ```bash
> rm -rf ~/.phelix
> ```

### Runtime dependencies

- **Go** toolchain (`go`) and/or **Rust** toolchain (`cargo`) — required to build
  apps. Phelix detects a missing toolchain and offers to install it
  automatically (Linux). For other OSes it prints manual instructions.
- **Docker** — only required for `phelix dockerize`.

Verify it runs:

```bash
phelix version
```

## Authentication

Authentication is **optional**. Logging in links your local Phelix server to
your account at `phelix.anophel.com` so that app data, resource metrics, and
events are pushed to your dashboard.

You can build, rebuild, start, restart, stop, and manage applications **without
logging in** — everything works locally. When you're not logged in, Phelix notes
during build/rebuild that no metrics or events will be sent to the dashboard
until you authenticate.

Once you log in, monitoring data starts flowing: events from commands you run
and, if the monitor daemon is running, resource metrics and logs. Apps you
created **before** logging in are synced to your dashboard at login time, so
nothing you built while offline is lost.

The session is stored locally at `~/.phelix/session.json`.

```bash
# Interactive
phelix auth login

# Non-interactive
phelix auth login --username <user> --apiKey <key>

phelix auth status     # show current session + expiry
phelix auth logout     # invalidate and remove the session
```

> **Tip:** want to use the CLI entirely offline, or just don't need the
> dashboard? Skip `auth login` — build/run commands work the same. Only the
> dashboard upload is skipped.

## Interactive Wizard

Phelix has two complementary interactive modes so you never have to memorize
exact argument order or flag names.

### Guided menu — `phelix wizard`

Run `phelix wizard` to open a survey-driven menu that walks you through any
task step by step: build, rebuild, roll back, start/stop/restart, status, logs,
list, environment variables, health checks, dockerize, deploy unlock, auth,
and version. Each choice routes to the same logic the plain subcommand uses, so
behavior is identical.

```bash
phelix wizard
```

### Auto-prompting on every command

When you run a command without a required positional argument (or key flag),
Phelix prompts for the missing input instead of erroring — **as long as stdin
is a terminal**.

```bash
phelix rebuild            # → prompts: which app?
phelix rollback           # → prompts: which app? then which version?
phelix env               # → prompts: set/get/list/unset/check, app, key…
phelix health set         # → prompts: which app? then the health path
phelix dockerize          # → prompts: which app? tag? push? registry?
```

Affected commands: `build`, `rebuild`, `rollback`, `start`, `stop`, `restart`,
`status`, `remove`, `dockerize`, `env`, `health *`, `deploy unlock`, and
`auth login` (now uses masked password input).

> **Scriptable by design.** Piping input or running in CI (non-TTY stdin)
> disables all prompts — commands return their normal usage/missing-argument
> error and exit code, so existing automation keeps working unchanged. The
> long-running daemons `monitor` and `proxy` (foreground) are not interactive.

## Core Workflow

A typical lifecycle:

```text
build    →  (rebuild --blue-green)  →  rollback    →  stop / remove
   \            |                          ^
    \           +-- versioned (v1, v2…) ----+
```

1. **Build** a project into a named, managed app and start it.
2. **Rebuild** after changes; optionally zero-downtime via blue-green or rolling.
3. Every build is **versioned** (`v1`, `v2`, …) and can be tagged.
4. Every successful build automatically records a **Build Report** (compiler, toolchain version, duration, cache status, binary size, commit) and compares it against previous comparable builds, surfacing binary-size and build-time regressions.
5. **Roll back** to any retained version if something breaks.
6. Monitor with `status`, `list`, `log`, and `health`.
7. Ship containers with `dockerize`.

## The PORT Contract

Phelix sets the `PORT` environment variable. Applications managed by Phelix should listen on the port provided by `PORT` rather than hardcoding a port.

```bash
phelix build api --port 4000
```

starts your application with `PORT=4000`, so it should listen on `:4000`. If the process runs but nothing listens on the requested port, Phelix reports a port-validation failure and suggests `phelix doctor`.

Go example:

```go
port := os.Getenv("PORT")
if port == "" {
    port = "3000"
}

log.Fatal(http.ListenAndServe(":"+port, router))
```

Three distinct port concepts:

| Concept | Example | Who owns it |
|---------|---------|-------------|
| Public proxy port | `:8080` | The Phelix proxy (clients connect here) |
| Internal application port | `:49152` / `:49153` | Phelix assigns per instance (blue/green get different ports) |
| `PORT` environment variable | `PORT=49152` | How Phelix tells each instance which internal port to use |

With blue-green deployment, each instance receives its own `PORT`; the proxy owns the single public port. A failed new deployment never touches the currently active instance.

### Project Configuration (`phelix.yaml`)

`phelix.yaml` is the project-level Phelix configuration file, stored in the
current project directory. It removes the need to repeat the application name,
port, health checks, and deployment strategy on every command.

#### Creating the configuration

```bash
phelix init
```

detects the project type (Go/Rust) and generates:

```yaml
# Phelix project configuration.
# CLI flags override these values (e.g. --port).
name: my-app
port: 8080
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

#### Full example

```yaml
name: api
port: 3000

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
```

#### Field reference

| Field | Required | Description |
|-------|----------|-------------|
| `name` | no* | Application name used by `build`, `rebuild`, `health`, `rollback`, etc. *Optional: when missing, `build` prompts for it (TTY) or fails with a clear error (scripts). |
| `port` | no | Public application port. Default `8080` when missing. |
| `health.endpoints[].name` | yes (per endpoint) | Endpoint name, e.g. `default`, `readiness`. Must be unique. |
| `health.endpoints[].path` | yes (per endpoint) | HTTP path on localhost, e.g. `/health`. Must start with `/`. |
| `health.endpoints[].interval` | no | Monitoring check interval (e.g. `10s`, `1m`). Default `10s`. |
| `health.endpoints[].retries` | no | Consecutive failures before marking DOWN. Default `3`. |
| `health.endpoints[].mode` | no | Deploy health tier: `auto` (default), `http`, `tcp-only`, `none`. |
| `deploy.strategy` | no | `classic` (default), `blue-green`, `rolling`, `canary`, or `progressive`. |
| `deploy.replicas` | no | Replica count for `rolling` (requires `deploy.strategy: rolling`). |
| `deploy.rollout.canary` | no | Traffic share (percent) for `canary` before promotion. Default `10`. |
| `deploy.rollout.duration` | no | Verification window for `canary` (e.g. `2m`). Default `30s`. |
| `deploy.rollout.steps[]` | no | Progressive steps: `traffic` (percent) and optional `duration` per step; must strictly increase and end at `100`. |
| `deploy.rollout.verification` | no | Regression thresholds: `interval`, `max_error_rate`, `max_error_delta`, `max_p95_factor` (see [Canary & progressive rollouts](#canary--progressive-rollouts)). |

The configuration file is fully optional. Projects without `phelix.yaml`
keep working exactly as before — every existing flag and prompt is unchanged.

#### Resolution order

`phelix build` (and the other project-aware commands) resolve values in this
order:

```text
CLI argument/flag  →  phelix.yaml  →  Phelix default  →  interactive prompt
```

With the full example above, running:

```bash
phelix build
```

resolves automatically — no prompts:

```text
Application: concurrency-lab-api
Port:        3000
```

Partial configurations work: a file with only `name:` prompts for the port
(in a TTY) or uses the default `8080`; a file with only `port:` prompts for
the name. No file at all means today's behavior (prompt or explicit args).

Explicit flags always win:

```bash
phelix build --port 8080        # uses port 8080 even though the config says 3000
phelix build my-custom-name     # uses my-custom-name even though the config has a name
```

#### Health endpoints

Endpoints declared in `phelix.yaml` are applied to the app's persisted health
configuration on every `phelix build` / `phelix rebuild`, so:

- `phelix health list <app>` and `phelix health status <app>` show them.
- Zero-downtime deploys use the first endpoint's `path`/`mode` as the deploy
  health tier (same as `phelix health set`).
- The `phelix health set/add/remove` commands keep working; a `health:` block
  in the config replaces the persisted endpoints at the next build/rebuild,
  while apps without a `health:` block keep whatever was set via the commands.

#### Deployment strategies

`deploy.strategy` selects which existing deployment path `phelix rebuild`
uses — it does not introduce a new deployment system.

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
`--canary`, `--port`), and `--strategy` overrides it for a single rebuild
without editing the file — the same one-off override the dashboard sends for a
remote rebuild. When nothing names a strategy, a rebuild keeps the one the app
is already deployed with (from `deploy.json`) instead of falling back to
classic: demoting a live blue-green/rolling app tears its instances down and
drops its deployment state, so it has to be asked for — `strategy: classic`
here, or `--strategy classic` for one rebuild. A rollout runs on the
blue-green topology, so an app whose last deploy was canary/progressive
inherits blue-green when nothing names a strategy; put `strategy: canary` or
`progressive` in `phelix.yaml` to make rollouts the app's default.
`phelix rollback` needs no configuration: it inspects the recorded deploy
state and automatically uses classic or zero-downtime rollback to match how
the app was actually deployed. `phelix proxy` is required for
blue-green/rolling/canary, exactly as with the flags.

#### Canary & progressive rollouts

A rollout deploys the new version *alongside* the stable one instead of
replacing it: the stable version keeps serving while the new version (the
canary) receives a share of live traffic through the proxy's weighted
routing. Every step of the plan is verified before the next share is applied,
and the final `100%` step is the promotion.

```bash
# One-shot canary: 5% of traffic for the verification window, then promote
phelix rebuild myapp --canary 5

# Same thing, resolved from phelix.yaml (deploy.strategy: canary)
phelix rebuild myapp

# Multi-step progressive rollout from phelix.yaml
phelix rebuild myapp --strategy progressive
```

`--canary` requires an existing deployment to serve as the stable baseline —
deploy with `--blue-green` (or `--replicas N`) first. A rollout over a
rolling fleet consolidates it to a single stable instance (its first replica)
for the comparison and promotion.

```text
→ step 1/4: routing 5% of traffic to the canary for 2m
    v12  ███████████████████  95%
    v13  █                     5%
  → monitoring canary for 2m (health and metrics every 5s)
    v13: 1,284 requests, 0.23% errors, p95 81ms
    v12: 24,391 requests, 0.31% errors, p95 74ms
  ✓ step 1/4 verified at 5% traffic
...
✓ traffic switched fully to slot green (zero downtime)
✓ progressive rollout of myapp complete: v13 serves 100% of traffic on slot green
```

Safety behavior:

* **Never route to an unhealthy canary.** The canary must pass the same
  deploy-tier health gate blue-green uses before it receives any traffic, and
  every step keeps probing it for the whole window.
* **Metric comparison.** The proxy counts requests, 5xx/failed responses and
  latency per backend, so each window compares the canary's error rate and
  p95 latency against the stable baseline using the `verification` thresholds
  above. With no per-backend metrics available the rollout degrades to
  health-only verification instead of guessing.
* **Automatic rollback to stable.** Any regression (health failure, error
  rate, latency) aborts the rollout: traffic switches back to the stable
  version at 100% atomically, the canary instance is stopped, and the failure
  is reported (`CANARY_REGRESSION`). This restore is built into the rollout —
  it does not need `--auto-rollback`, which still covers the cases where the
  restore itself fails.
* **Cancellation.** Ctrl-C aborts the rollout through the same restore path
  (the stable version keeps 100% of traffic); the cleanup runs even though
  the deploy was interrupted.
* **Crash recovery.** A rollout interrupted by a crash leaves the stable
  version serving; the next deploy restores stable routing and reclaims the
  leftover canary before doing anything else.
* **Success is durable.** The rollout is only reported successful after the
  final promotion is complete: state persisted, version promoted in
  `versions.json`, old instance drained.

#### Validation

The configuration is validated before it can affect a build or deploy — an
invalid file fails the command with a `CONFIGURATION_ERROR` instead of being
ignored:

```text
Configuration error: invalid deploy.strategy "foobar"
Hint: expected one of: classic, blue-green, rolling, canary, progressive
```

Validated: YAML syntax, port range, strategy name, `replicas` (≥ 1, rolling
only), endpoint names (required, unique), paths (must start with `/`),
durations (`10s`, `1m`, …), modes (`auto`, `http`, `tcp-only`, `none`), and
the rollout plan: traffic percentages (whole numbers, `25` or `"25%"`), each
step within `1-100`, strictly increasing shares ending at the `100%`
promotion, non-negative durations, `canary` within `1-99`, and verification
thresholds (positive interval, error rates within `0-100`, p95 factor ≥ 1).

#### Related commands

- `phelix init` — generate the file (scriptable: `--name`, `--port`, `--yes`).
- `phelix doctor` — reports whether `phelix.yaml` is found and valid.
- `phelix build` / `phelix rebuild` — read name, port, health, and strategy.

## Command Reference

Every command below also supports [interactive auto-prompting](#interactive-wizard):
run it without a required argument and Phelix will ask for it (when stdin is a
terminal).

### Building & rebuilding

#### `phelix build <NAME> [flags]`
Compiles the project in the current directory (auto-detects Go or Rust), starts
it on the given port, and records a **new versioned build**. A **Build Report**
with regression analysis is printed automatically after every successful build
(see [Automatic Build Reports](#automatic-build-reports)).

`NAME` and `--port` are resolved in this order: CLI argument/flag →
[`phelix.yaml`](#project-configuration-phelixyaml) → default `8080` →
interactive prompt. When the config file supplies both, the command runs
without asking anything.

| Flag | Default | Description |
|------|---------|-------------|
| `--port, -p` | `8080` | Port to run the app on (not required for matrix builds — they never start an instance) |
| `--build-arg, -a` | — | Extra args passed to the build tool (repeatable); in matrix mode they are passed to every `go build` / `cross build` |
| `--tag` | — | Human label stored with the version (e.g. `"hotfix-auth"`) |
| `--no-upload` | `false` | Don't sync app info to the server |
| `--debug` | `false` | Verbose build/tool output |
| `--matrix` | `false` | Matrix mode: build versions × platforms (see [Matrix builds](#matrix-builds); also activates via the phelix.yaml matrix profile) |
| `--go-versions` | — | Go versions, e.g. `1.22,1.23,1.27` |
| `--rust-versions` | — | Rust versions, e.g. `1.77,1.78.2` |
| `--platforms` | — | Targets, e.g. `linux/amd64,linux/arm64` |
| `--matrix-concurrency` | 3 | Max parallel matrix builds |
| `--matrix-dry-run` | `false` | Print the plan without building |

```bash
phelix build myapp -p 8080 --tag "v1.2.3"
```

#### `phelix init`

Detects the project type (Go/Rust) and creates `phelix.yaml` with the
application name, port, a default health endpoint, and the classic deploy
strategy (see [Project Configuration](#project-configuration-phelixyaml)).
Prompts interactively in a TTY; fully scriptable with flags:

```bash
phelix init --name api --port 3000 --yes
```

Never modifies application source code. If a hardcoded listen port is detected, prints a warning and points to `phelix doctor`.

#### `phelix doctor`

Diagnoses whether the current project is Phelix-compatible: project detection, toolchain, build tools, `phelix.yaml`, and — most importantly — whether the application listens on `PORT` instead of a hardcoded port.

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

Exits non-zero when a critical check fails. Inconclusive detection (no listener found, no `PORT` usage) is reported as ⚠ warning, not a false failure. Diagnostic only — never rewrites source code.

#### `phelix rebuild <ID|AppName> [flags]`
Rebuilds an existing app from its source directory. Supports zero-downtime
deploy. Without an argument, the app is taken from `phelix.yaml` (`name:`)
when it exists in the current directory, otherwise an interactive picker is
shown. The deployment path is resolved in this order: explicit
`--blue-green` / `--replicas` → `--strategy` → the config's
[`deploy.strategy`](#project-configuration-phelixyaml) → the strategy the app
is currently deployed with → classic. An app already running blue-green or
rolling therefore stays on it unless a classic rebuild is asked for
explicitly; a rolling rebuild inherited this way keeps the replica count the
app is running at.

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

```bash
phelix rebuild myapp --blue-green
phelix rebuild myapp --replicas 3

# Canary: 5% of traffic, verified, then promoted
phelix rebuild myapp --canary 5

# One-off override: deploy classic once, even though phelix.yaml says rolling
phelix rebuild myapp --strategy classic

# Rolling once; replica count comes from deploy.replicas, else 1
phelix rebuild myapp --strategy rolling
```

`--strategy rolling` combined with `--replicas N` uses N replicas. `--canary`
cannot be combined with `--strategy`, `--blue-green` or `--replicas` — it
already names a strategy. An unrecognized value fails with
`INVALID_ARGUMENT` (exit code `2`) before anything is built. This is the same
override the dashboard sends for a remote rebuild — see
[gRPC monitoring](docs/grpc-monitoring.md#24-one-off-deployment-overrides)
(`canary`/`progressive` overrides resolve their plan from the app's
`phelix.yaml`). See [Canary & progressive
rollouts](#canary--progressive-rollouts) for the rollout behavior.

#### Automatic Build Reports

Every successful native Go/Rust build (`build`, `rebuild`, zero-downtime deploys) automatically prints a **Build Report** and records it with the version metadata — no flags, no configuration, no network:

```text
→ Build Report
  Application   api
  Version       v12
  Compiler      Go 1.27
  Duration      31.2s
  Cache         COLD (go-build-cache)
  Binary        14.8 MB (linux/amd64)
  Commit        8f31c2a
  ────────────────────────────────────────────────

  Regression Analysis
  Compared against 1 previous comparable build.

  Binary size
    Previous      14.1 MB
    Current       14.8 MB
    Change        +0.7 MB (+4.9%)

  Build duration
    Comparison skipped:
    cache mode differs from the previous comparable build (COLD → HIT)
```

The report captures: application, version, language, compiler/toolchain version (`go version` / `rustc --version`), build start/end time, duration, binary size and target platform, cache status (`COLD` = real compilation happened, `HIT` = served from the compiler's build cache), git commit when available (`unavailable` outside a git repository), and the build arguments you passed via `--build-arg`.

**The critical cache rule:**

> Build duration comparisons are only made between comparable cache modes. A cold build is never directly compared against a cache-hit build for duration regression — the report explicitly says `Comparison skipped` instead of inventing a misleading percentage.

> Binary size remains a valid regression signal across cache states when the artifact, toolchain, platform, and matrix context are comparable.

**Regression analysis** compares the current build against the previous **5 comparable builds** (same application, language, toolchain, target platform, artifact type — cache state ignored for size, matched for duration). Alerts fire on:

- **Binary size**: growth ≥ 1 MB **or** ≥ 5% versus the previous comparable build, plus a `N-build average` baseline when enough history exists.
- **Build duration**: slowdown ≥ 25% **and** ≥ 2s versus a previous build in the *same* cache mode (thresholds avoid noisy micro-changes).

Regression analysis is pure observability: missing history never fails a build, and a reporting problem only prints a warning.

#### `phelix build-report <AppName> [flags]`

Read-only inspection of stored build reports — never triggers a build, works fully offline:

```bash
phelix build-report myapp            # 5 most recent reports
phelix build-report myapp --limit 10
```

```text
→ Build reports for 'myapp' (2 most recent)
→ v2  2026-08-26T03:25:00Z
    Compiler   Go 1.27
    Duration   5.2s
    Cache      HIT (go-build-cache)
    Binary     14.9 MB (linux/amd64)
    Commit     8f31c2a
    Args       -trimpath
→ v1  2026-08-26T03:20:00Z
    ...
```

Versions recorded before build reporting existed show `no build report metadata`.

### Versioning & rollback

#### `phelix rollback <AppName> [flags]`
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

**Interactive picker:** in a TTY, `phelix rollback <App>` without `--to` opens an
interactive picker instead of silently choosing the previous version. It lists
valid rollback targets newest → oldest (current version and versions whose
binary is missing are excluded), shows built-time metadata per entry, and after
you press Enter displays the rollback preview and asks for confirmation before
executing. `Esc`/`Ctrl-C` cancels cleanly without touching the deployment.

**Explicit mode:** `--to v7` resolves and executes the requested target exactly
as before — no picker, no confirmation prompt — keeping scripts and automation
deterministic. In non-interactive environments (CI, redirected stdin) the
picker is skipped and rollback falls back to the previous-version default.

```bash
phelix rollback myapp              # interactive picker (TTY), else previous version
phelix rollback myapp --to v3
phelix rollback myapp --to "hotfix-auth"
phelix rollback myapp --list
phelix rollback myapp --to v3 --dry-run   # preview only — no changes
```

#### Rollback reason (`--reason`)
The reason becomes part of the rollback history/audit record, so `phelix
rollback history` explains *why* each rollback happened:

```bash
phelix rollback myapp --to v7 --reason "Login endpoint returning 500"
```

The reason is optional; `--dry-run` displays it in the plan but persists
nothing. Explicitly supplied-but-empty reasons are rejected
(`rollback reason cannot be empty`), whitespace is collapsed, and the text is
capped at 500 characters. It is serialized through `encoding/json` — never
concatenated — so it cannot corrupt the structured history or inject log
lines. In the interactive picker the reason is requested after the target
version is chosen (Enter skips it); an explicit `--reason` never re-prompts.

#### Rollback verification (`--verify`)
A rollback should not count as successful merely because the process started.
With `--verify <duration>`, Phelix observes the application for the requested
period **after** the rollback reaches its committed, traffic-serving state
(target loaded, environment restored, instance healthy, proxy switched,
deployment state persisted) and fails if it does not remain healthy:

```bash
phelix rollback myapp --to v7 --verify 30s
```

```text
→ Verifying rollback stability...
  5s    ✓ healthy
  10s   ✓ healthy
  ...
✓ Rollback remained healthy for 30s
```

Semantics:

* Verification observes the instances **actually serving traffic** — the
  active blue-green slot, all rolling replicas, or the classic process — never
  a drained slot.
* It reuses the existing tiered health checks and the per-app health
  configuration (`phelix health set`); the duration is the observation
  window, not a request timeout, and it is parsed as a real Go duration
  (`30` is rejected — use `30s`).
* **Verification failure is distinct from rollback execution failure.** The
  CLI prints `Rollback execution: SUCCESS / Verification: FAILED` and exits
  **23** (`ROLLBACK_VERIFY_FAILED`), while an execution failure exits **22**
  (`ROLLBACK_FAILED`). History records the execution as `SUCCESS` with a
  verification block (`passed` / `failed` / `cancelled`).
* `Ctrl+C` during the window cancels the observation (history: `cancelled`);
  the completed rollback stays active. Phelix never rolls forward/backward on
  its own — recovery is your call.
* Omitting `--verify` preserves the existing rollback behavior exactly; no
  extra delay is added.
* `--dry-run --verify 30s` shows the planned window and performs nothing.

Combined:

```bash
phelix rollback myapp \
  --to v7 \
  --reason "Login endpoint returning 500" \
  --verify 30s
```

#### Automatic rollback on failed deployment (`phelix rebuild --auto-rollback`)
Add `--auto-rollback` to `phelix rebuild` and a deploy-phase failure restores
the previous known-good version automatically — the same path a manual
rollback takes, so locks, health checks, proxy switching, state reconciliation
and history are shared, not duplicated:

```bash
phelix rebuild myapp --blue-green --auto-rollback
phelix rebuild myapp --replicas 4 --auto-rollback
```

```text
✗ v13 failed health checks
→ Automatic rollback enabled
  → automatic rollback: restoring v12
  ✓ v12 started, healthy and serving traffic
✓ Deployment rolled back automatically
```

Behavior:

* **Failure boundaries.** Build/compile failures never trigger a rollback
  (no new version was recorded, nothing was displaced). Deployment-phase
  failures do: instance start failure, failed health checks, proxy switch
  failure, and a partially-completed rolling rollout.
* **Canary/progressive.** A regression during a rollout is handled by the
  rollout itself: the stable version is restored to 100% of traffic before
  the command returns (`CANARY_REGRESSION`), so there is normally nothing
  left for `--auto-rollback` to do — it reports "kept serving" instead of
  faking a rollback. It still covers the corner cases where the rollout's
  own restore fails.
* **Blue-green.** A failed candidate is killed *before* the traffic switch,
  so the previous version usually kept serving the whole time — Phelix reports
  that ("kept serving") instead of faking a rollback. No history entry is
  written for a rollback that did not happen.
* **Rolling.** If the rollout failed after some replicas switched to the new
  version, recovery redeploys the known-good version over the fleet one
  replica at a time, preserving availability. If it failed at the first
  replica, the old fleet is still intact and nothing is redeployed.
* **Classic.** The known-good binary is copied back and restarted (brief
  downtime is inherent to classic — Phelix does not claim zero downtime).
* **Last known good is authoritative.** The restore target is the version
  versions.json currently promotes (a failed deploy never promotes itself),
  restricted to versions whose binary still exists — never "current - 1".
  With no known-good version, Phelix says so instead of fabricating a target.
* **History.** Automatic recoveries appear in `phelix rollback history` with
  the `SOURCE` column set to `automatic` and a reason derived from the
  deployment failure ("Deployment v13 failed: …"). Manual rollbacks show
  `manual`.
* **Recovery can fail too.** If the previous version cannot be restored
  safely, Phelix prints the degraded state explicitly and exits **24**
  (`AUTO_ROLLBACK_FAILED`) — it never claims success. A recovered deployment
  still exits **21** (the deployment itself failed; the output and history
  say recovery succeeded).
* The rollback is triggered exactly once per failed deployment and cannot
  recurse: recovery failures are terminal, never new rollback triggers.

Interactive picker example (actual versions and metadata depend on the application):

```text
Rollback 'myapp'

Current: v12

Select version to rollback to:

❯ v11   hotfix-auth           2 min ago
  v10   release-2.4.0         1 hour ago
  v9    stable                yesterday
  v8    —                     3 days ago

↑/↓ select   Enter continue   Esc cancel
```

Moving the cursor updates a details footer for the focused version, rendered
from stored build metadata only (no process is started, no network check runs
while navigating; values that are not stored show `—`):

```text
❯ v11   hotfix-auth           2 min ago
  v10   release-2.4.0         1 hour ago

v11
├── Tag: hotfix-auth
├── Commit: 8f31c2a
├── Built: 2 min ago
├── Binary: 14.8 MB
├── Health: —
└── Deploy: blue-green
```

Pressing Enter shows the same rollback preview as `--dry-run` followed by a
`Proceed with rollback?` confirmation before anything executes.

For interactive inspection use the picker; for deterministic automation always
pass an explicit `--to`.

#### `phelix rollback history <AppName> [--limit N]`
Shows the recorded rollback outcomes for one application, newest first. Every
rollback attempt that reaches execution is recorded — successes and failures —
at the moment the rollback transaction completes, together with the deployment
mode actually used for that rollback (`classic`, `blue-green`, `rolling`). The
mode is a historical fact: later re-deploys of the app never rewrite old
records. `FROM`/`TO` are the versions of that transition, not the app's
current version.

| Flag | Default | Description |
|------|---------|-------------|
| `--limit` | `20` | Maximum number of entries to show (must be a positive number) |

```text
Rollback History — myapp

TIME                  FROM   TO     STATUS   MODE         REASON
2026-09-06 14:20:31   v12    v7     SUCCESS  blue-green   Login endpoint returning 500
2026-09-06 13:11:02   v13    v12    SUCCESS  rolling      API regression
2026-09-02 09:13:12   v9     v8     FAILED   rolling      Database migration issue
2026-08-28 18:42:09   v8     v6     SUCCESS  classic      —
```

Notes:

* An app with no rollbacks shows `No rollback history found.` — normal state,
  not an error.
* `--dry-run` previews never record history; cancelling the interactive
  picker (`Esc`) never records history either, and is not a failure.
* Malformed legacy lines in the history file are skipped with a short
  warning; they are never silently rewritten.
* `REASON` shows the recorded `--reason`, or `—` for reason-free and
  pre-reason records (old history entries load unchanged and are never
  rewritten to add an empty reason).
* When `--verify` was requested, each record also carries a verification
  block on disk (`"verification": {"requested": true, "duration": "30s",
  "status": "passed"}` with status `passed` / `failed` / `cancelled`). A
  failed window renders as `VERIFY_FAILED` while `STATUS` stays `SUCCESS` —
  execution outcome and verification outcome are kept separate so history
  always tells the truth about what happened.
* History is stored per app as JSON Lines at
  `~/.phelix/apps/<AppName>/rollback_history.jsonl`.

```bash
phelix rollback history myapp
phelix rollback history myapp --limit 50
```

### Lifecycle

| Command | Description |
|---------|-------------|
| `phelix start [<ID\|AppName>] [--port P] [--ensure]` | Start an app (or all apps if none given). `--ensure` builds if missing and rebuilds if startup fails. |
| `phelix stop <ID\|AppName>` | Stop a running app. |
| `phelix restart <ID\|AppName>` | Restart an app. |
| `phelix status <ID\|AppName>` | Show status: PID, uptime, RAM/CPU, version, deploy mode, proxy routing. |
| `phelix list` | Table of all apps with version, status, deploy, and proxy columns. |
| `phelix log [<ID\|AppName>]` | Tail app logs (last 10 lines + live stream). No arg → Phelix's own logs. |
| `phelix remove <ID\|AppName>` | Stop and delete an app from management. |

### Encrypted environment variables

`phelix env` manages per-app secrets encrypted at rest with **AES-256-GCM**.
The encryption key lives at `~/.phelix/master.key` (auto-generated, `0600` permissions).

```bash
phelix env set    MyApp DATABASE_URL=postgresql://localhost/db
phelix env get    MyApp DATABASE_URL      # sensitive values are masked
phelix env list   MyApp                   # values shown as ***REDACTED***
phelix env unset  MyApp DATABASE_URL
phelix env check  MyApp DATABASE_URL      # exit-status friendly existence check
```

Encrypted values are injected into the app process at start, and snapshotted
alongside each versioned build so a rollback restores the matching env.
Keys containing `SECRET`, `KEY`, `TOKEN`, or `PASSWORD` are automatically masked
in logs and command output.

### Health checks

`phelix health` configures HTTP health endpoints and the deploy-time health tier.

```bash
phelix health set <App> --path /health --interval 10s --retries 3 --mode auto
phelix health add  <App> --name "API" --url https://api.example.com/health
phelix health list   <App>
phelix health remove <App> --name "API"
phelix health status <App> [--watch]   # --watch = live terminal dashboard
```

**Deploy health tiers** (used by blue-green/rolling to decide when an instance is
safe to receive traffic):

| Tier | Trigger | "Alive" means |
|------|---------|---------------|
| Tier 1 | Explicit `--path` set | HTTP 2xx on that path |
| Tier 2 | No path, app speaks HTTP | Any HTTP response (even 404/500) |
| Tier 3 (TCP) | Not HTTP, or `--mode tcp-only` | TCP port accepts connections |
| Tier 3 (PID) | Worker/daemon, or `--mode none` | Process PID still exists |

`--mode` options: `auto` (default), `http`, `tcp-only`, `none`.

### Zero-downtime reverse proxy

The `phelix proxy` daemon binds each app's public port and routes traffic to the
active internal instance. Blue-green/rolling deploys atomically switch the
target through a control socket — no dropped connections.

```
Client → :8080 [phelix proxy]  ──atomic target──→ blue  :9001
                                               ↘ green :9002
```

#### Prerequisites
1. Your app must listen on the port given by the `PORT` environment variable (Phelix sets this for each internal instance).
2. Prefer a real health endpoint so deploys use Tier 1 checks:
   ```bash
   phelix health set myapp --path /health
   ```
   Without one, Phelix falls back to Tier 2 (any HTTP response) or Tier 3 (TCP only) and prints a warning.
3. One-time migration: an app currently running outside the proxy (started via a classic build/rebuild) binds the public port itself, so the first `--blue-green` / `--replicas` deploy cannot enrol it. Stop the app once (`phelix stop <app>`) and deploy — the proxy takes over the public port, and every deploy after that switches targets with zero downtime.

```bash
phelix proxy              # start in background (detaches and returns)
phelix proxy status       # show enrolled apps + routing
phelix proxy stop         # shut the daemon down (drains up to 30s)
phelix proxy --foreground   # run attached (for process supervisors / systemd)
```
`phelix rebuild --blue-green` / `--replicas` will also auto-start the daemon if it is not already running.
Enrolled apps and their active targets are **persisted** (`~/.phelix/proxy-state.json`) and restored automatically when the daemon restarts, so backend instances keep serving through daemon crashes or reboots without any redeploy.

#### Blue-green deploy
Builds a new binary, starts it on the inactive slot (blue ↔ green), waits until healthy, then atomically switches the proxy. The previous instance is drained and stopped. A deploy that loses the proxy mid-flight shuts down its unproven new instance instead of leaking it, and slots left behind by crashed deploys are reclaimed on the next run.
```bash
phelix rebuild myapp --blue-green
phelix rebuild myapp --blue-green --port 8080
```

#### Rolling deploy
Replaces N replicas one at a time. Each replacement starts on a fresh internal port **alongside** the instance it replaces and joins the proxy only after passing its health check; the old instance is then drained from rotation and stopped, so no request is ever routed to a dead port during the roll. Shrinking (`--replicas N` smaller than before) drains the surplus replicas at the end of the rollout.
```bash
phelix rebuild myapp --replicas 3
```

#### What you see in `list` / `status`
- **Deploy**: `blue-green (blue|green)` or `rolling (×N)`, or `-` for classic stop→start rebuilds
- **Proxy**: `on :<public> → <slot>` when enrolled, otherwise `off`

#### Failure behaviour
If the new instance fails its health check, the deploy **aborts**, the new instance is killed, and the currently active instance is left untouched — public traffic keeps flowing.

### Versioned builds and rollback

Every successful build — whether via `phelix build`, `phelix rebuild`, or zero-downtime deploy — creates a numbered version (`v1`, `v2`, `v3`, ...) rather than overwriting. Versions store both the binary and its paired encrypted env snapshot, so rollback always restores a known-good binary + env pair — never binary-only.

#### Build Report metadata

Each version row in `versions.json` also carries an additive `build_report` object with the metrics captured during that build — language, compiler/toolchain version, build start/end timestamps, duration, cache status and cache source, plus per-artifact details (type, size, target platform):

```json
{
  "version": 12,
  "built_at": "2026-08-26T03:20:00Z",
  "size_bytes": 15518924,
  "git_commit": "8f31c2a...",
  "is_current": false,
  "build_report": {
    "language": "go",
    "compiler": "go",
    "compiler_version": "1.27",
    "started_at": "2026-08-26T03:19:29Z",
    "ended_at": "2026-08-26T03:20:00Z",
    "duration_ms": 31200,
    "cache": { "status": "cold", "source": "go-build-cache" },
    "artifact": { "type": "binary", "size_bytes": 15518924, "platform": "linux/amd64" }
  }
}
```

The field is optional and additive: versions recorded by older Phelix releases (and Docker-image versions) simply lack it. Missing report metadata only makes that version unavailable for regression comparisons — rollback, rollback listing, status, version loading, retention/pruning and the `current` symlink logic all keep working unchanged. A malformed optional `build_report` payload degrades gracefully to "no report" instead of breaking the version index.

#### Build-and-deploy ordering guarantee

Versions use a two-phase commit to ensure safety:

1. **Build succeeds** → version is created on disk (`builds/vN/binary`, `env/vN.enc`) and recorded in `versions.json` with `is_current: false`.
2. **Deploy succeeds** (start/health check passes) → `PromoteVersion` flips `is_current: true` and updates the `current` symlink.
3. **Deploy fails** → the version exists on disk for inspection or retry, but `is_current` stays `false` and the `current` symlink is never moved. The active running instance is untouched.

This means you can always inspect a failed build's artifacts, but a broken deploy can never corrupt the "current" pointer.

```
~/.phelix/apps/myapp/
├── builds/v1/binary
├── builds/v2/binary
├── builds/v3/binary
├── env/v1.enc
├── env/v2.enc
├── current -> builds/v3    (symlink, updated only after proxy switch succeeds)
├── versions.json           (metadata per version)
└── rollback.log            (audit trail)
```

#### `phelix rollback <AppName>`
Roll back to a previous version. The rollback path depends on how the app was deployed:

- **Zero-downtime apps** (built with `--blue-green` or `--replicas`): rollback goes through the same deploy path — health check, proxy switch, graceful shutdown — so you get the same zero-downtime guarantee.
- **Classic apps** (built with plain `phelix build` / `phelix rebuild`): rollback stops the current instance, copies the versioned binary into place, and starts it. This is a brief downtime rollback (stop → start).

```bash
phelix rollback myapp              # interactive picker (TTY), else previous version
phelix rollback myapp --to v2      # roll back to a specific version (no picker, no prompt)
phelix rollback myapp --to 3       # version number without 'v' prefix also works
phelix rollback myapp --to hotfix-auth-bug   # roll back by tag name
```

The `--to` flag accepts either a version ID (`v3`, `3`) or a unique tag name. If a tag matches exactly one version, it resolves automatically. If a tag matches zero or more than one version, an error is returned — use a version ID to disambiguate.

**Interactive rollback:** when run in a terminal without `--to`, `phelix rollback myapp` opens an interactive picker listing valid rollback targets (newest → oldest; the current version and versions whose binary is missing are excluded). After you select a version, the rollback preview is shown and a confirmation prompt gates execution:

```text
Rollback 'myapp'

Current: v12

Select version to rollback to:

❯ v11   hotfix-auth           2 min ago
  v10   release-2.4.0         1 hour ago
  v9    stable                yesterday
  v8    —                     3 days ago

↑/↓ select   Enter continue   Esc cancel
```

`Esc` (or `Ctrl-C`) cancels cleanly — `Rollback cancelled.` is printed, nothing is deployed, and this is not reported as a rollback failure. Without a TTY (CI, scripts, redirected stdin) the picker never opens and rollback falls back to the previous-version default, so automation never hangs waiting for input. Prefer the picker for interactive inspection and an explicit `--to` for deterministic automation.

#### `phelix rollback <AppName> --list`
Show all retained versions with metadata.
```bash
phelix rollback myapp --list
```
Output table columns:
- **Version** — `vN` label
- **Tag** — optional label (e.g. `hotfix-auth-bug`), if provided via `--tag`
- **Commit** — git commit hash (if available at build time)
- **Built** — build timestamp (RFC 3339)
- **Size** — binary size on disk
- **Current** — whether this version is actively serving traffic
- **Prune soon** — whether this version would be removed after the next build (based on retention policy)

#### `phelix rollback <AppName> --dry-run`
Preview exactly what a rollback **would** do — target resolution, strategy,
traffic transition, health checks, and the step-by-step plan — without making
any changes. Nothing is started, stopped, switched, promoted, or written: no
process, proxy, `versions.json`, `deploy.json`, `current` symlink, or
`rollback.log` mutation, and no rollback audit entry is recorded. It is safe to
run any number of times, including while the app serves traffic.

The preview is a **plan and validation preview**, not a guarantee: it reads the
same metadata the real rollback resolves (current/target version, deploy mode,
env snapshot presence, health-tier configuration), validates that the target
artifact exists, and reports warnings — but it does not start instances,
perform live health checks, or predict ports that are only assigned at startup.

The same target resolution as a real rollback applies (`--to` with `vN`, `N`,
or a tag; default previous version), so `--dry-run` fails on an invalid or
missing target with the same error codes a real rollback would return. (Output
below is illustrative — actual values come from your app's state.)

```bash
phelix rollback myapp --to v7 --dry-run
```
```text
→ Rollback Preview

  Application    myapp
  Current        v12
  Target         v7
  Built          2026-08-31 14:22:10
  Commit         8f31c2a
  Deploy Mode    blue-green
  Health Check   Tier 1 (/health, 2xx required)
  Environment    v7 snapshot available

Changes:
  Version        v12 → v7
  Binary         15.2 MB → 14.8 MB
  Environment    v7 snapshot available

Traffic:
  Public         :3000
  Current        green
  Target         blue

Rollback Plan:
   1. Ensure the proxy daemon is running
   2. Load the v7 artifact (.../builds/v7/binary)
   3. Restore the v7 environment snapshot (env/v7.enc)
   4. Start the new instance on the inactive slot blue (internal port assigned at startup)
   5. Run health checks against the new instance
   6. Switch proxy traffic on public port 3000 from slot green to slot blue
   7. Promote v7 as current (versions.json + current symlink)
   8. Drain and stop the old slot green instance (grace 30s)

✓ No changes will be made.
```

For **classic** apps the preview shows the stop→start plan and an explicit
`Downtime: Expected yes` line; for **rolling** apps it lists one replacement
step per replica, in the order the real rollback replaces them. If validation
fails (missing binary, unknown version, tag ambiguity, target == current), the
preview reports the same structured error a real rollback would return — with
nothing modified. Warnings (e.g. a missing env snapshot, a large rollback
distance, a deploy lock held by another operation) are shown in a `Warnings:`
section without blocking the preview.

#### How rollback works

Rollback is split into two phases: **resolve + plan**, then **execute**. The
CLI first resolves the target version and deployment strategy and validates
the target artifact (shared by both the preview and the real path); a real
rollback then executes the plan, `--dry-run` renders it and exits.

**Zero-downtime path** (blue-green / rolling apps):
1. `ResolveVersionOrTag` resolves the `--to` argument to a concrete version ID
2. `ExistingVersionSource` resolves the binary and env paths for the target version
3. A new instance starts on the inactive slot (blue or green)
4. Tiered health checks verify the instance is healthy
5. The proxy atomically switches traffic to the new instance
6. The old instance is gracefully drained and stopped
7. The `current` symlink and `versions.json` are updated

If the rollback target fails its health check, the rollback **aborts** and the active instance is left untouched — identical to a failed forward deploy.

**Classic path** (apps built without `--blue-green` / `--replicas`):
1. `ResolveVersionOrTag` resolves the target version
2. The current instance is stopped
3. The versioned binary is copied to the app's expected location
4. The app is started via `app.Manager.StartApplication`
5. The version is promoted (`is_current` set to true, `current` symlink updated)

If the start fails, the version exists on disk but `is_current` stays false — you can retry without a broken "current" pointer.

#### Rollback safety
- **Dry-run preview**: `--dry-run` shows the full plan with zero mutations — verify before you commit (see above)
- **Concurrent protection**: A deploy lock prevents rollback from racing with another deploy or rollback on the same app
- **Versioned env**: Binary and env are paired per version; rollback always restores both
- **Audit log**: Every rollback attempt (success or failure) is recorded in `~/.phelix/apps/<AppName>/rollback.log`

#### Retention policy
Old versions are automatically pruned after each successful build, keeping the last **5** versions by default. The currently active version is never pruned, even if it falls outside the retention window. The retention count is configurable per plan tier (Free: 3, Pro: 10, Enterprise: unlimited).

### Docker image building

Phelix can build optimized multi-stage Docker images for Go and Rust projects. Language is auto-detected from `go.mod` (Go) or `Cargo.toml` (Rust).

#### `phelix dockerize <AppName>`

```bash
phelix dockerize myapp --tag v1.0.0
phelix dockerize myapp --tag v1.0.0 --push --registry ghcr.io/myuser
phelix dockerize myapp --tag v1.0.0 --build-arg VERSION=1.0.0
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
| `--matrix` + family | Matrix Docker builds — `--go-versions`/`--rust-versions`/`--platforms`, `--multi-arch-tag` (requires `--push`), `--push-partial`, `--matrix-concurrency`, `--matrix-retries`, `--matrix-dry-run` (see [Matrix builds](#matrix-builds)); a `matrix:` profile in `phelix.yaml` activates the Docker matrix too |

#### How it works

**Language detection:** checks for `go.mod` (Go) or `Cargo.toml` (Rust); fails with a clear error if neither or both are found.

**Dockerfile generation (if none exists):** multi-stage builds optimized for Docker layer caching — dependency-heavy layers are cached separately from source code. The toolchain images are parameterized (`ARG GO_VERSION=1.23` / `ARG RUST_VERSION=1.80`), so you can pin a different toolchain per build without editing the file:

```bash
phelix dockerize myapp --build-arg GO_VERSION=1.27
```

*Go:*
1. **Builder stage** — `FROM golang:${GO_VERSION}-alpine` → `COPY go.mod go.sum` → `go mod download` → `COPY . .` → `go build`. Dependencies are cached before source is copied; `CGO_ENABLED=0` for a fully static binary.
2. **Runtime stage** — `FROM scratch` with just the binary. Smallest possible image.

*Rust:*
1. **Dependency cache stage** — `FROM rust:${RUST_VERSION}-slim`, copies `Cargo.toml`/`Cargo.lock`, builds a dummy `main.rs` to compile and cache all dependencies.
2. **Real build stage** — copies real source; only the app's own code recompiles.
3. **Runtime stage** — `debian:bookworm-slim` with `ca-certificates`.

> **If a Dockerfile already exists in the project directory, it is used as-is** — Phelix never overwrites a user-provided Dockerfile.

**`.dockerignore` generation (if none exists):** excludes `.git`, `.env` files, build artifacts, logs, IDE configs, and Phelix internals.

**Build output:** Docker's build progress streams live to the terminal.

**OCI metadata labels:** `org.opencontainers.image.version`, `.revision` (git commit), `.created` (build timestamp).

**Version integration:** each `dockerize` call records the image reference in `versions.json` — the same versioning system used by `phelix rollback` — so future rollback logic can support `docker run <image>` as a deploy source without redesigning the schema.

**Push to registry:** `--push` pushes to a configurable registry. Credentials are stored using the same AES-256-GCM encrypted mechanism as environment variables. If not logged in, a clear error tells you to run `docker login` first.

#### Docker-compose generation

```bash
phelix dockerize myapp --tag v1.0.0 --with-compose --depends-on redis,postgres
```
Produces a ready-to-use `docker-compose.yml` with the app's service plus requested sidecars (e.g. Redis on port 6379 using `redis:7-alpine`; PostgreSQL on port 5432 using `postgres:16-alpine`). Supported sidecars: `redis`, `postgres`, `mysql`, `mongodb`, `rabbitmq`.

> **Important:** Phelix only *generates* the compose file. It does **not** manage the lifecycle (start/stop/health) of compose-defined services — use `docker compose up -d` directly.

## Run Phelix in Docker

When Phelix itself runs in Docker, each Phelix container is an independent
**agent**. Its identity belongs to the Phelix runtime, not to the physical or
VM host and not to the application it manages. This lets several Phelix
containers on one host report to the same backend without colliding.

The identity rule is:

```text
machine_id ≠ agent_id ≠ app_id ≠ instance_id
```

- `machine_id` identifies a physical or virtual host. Phelix does not use it
  as its agent identity.
- `agent_id` identifies one persistent Phelix installation/runtime.
- `app_id` identifies an application independently of its agent.
- `instance_id` identifies a particular deployed or running instance.

### Persistent agent data

Phelix creates `/var/lib/phelix/agent-id` on first start and reuses the UUID on
every later start. Its application registry (`apps.json`) is stored in the same
directory, so application IDs are persistent too. Mount a **different
persistent volume for every Phelix container**. Do not mount
`/etc/machine-id` into the containers, and never share one `/var/lib/phelix`
volume between them.

```yaml
services:
  billing:
    image: <your-phelix-image>
    command: ["phelix", "monitor"]
    hostname: billing-agent
    volumes:
      - phelix-billing-data:/var/lib/phelix

  auth:
    image: <your-phelix-image>
    command: ["phelix", "monitor"]
    hostname: auth-agent
    volumes:
      - phelix-auth-data:/var/lib/phelix

  user:
    image: <your-phelix-image>
    command: ["phelix", "monitor"]
    hostname: user-agent
    volumes:
      - phelix-user-data:/var/lib/phelix

  admin:
    image: <your-phelix-image>
    command: ["phelix", "monitor"]
    hostname: admin-agent
    volumes:
      - phelix-admin-data:/var/lib/phelix

volumes:
  phelix-billing-data:
  phelix-auth-data:
  phelix-user-data:
  phelix-admin-data:
```

Start the services normally:

```bash
docker compose up -d
docker compose logs -f billing
```

The volume determines recreation behavior:

| Operation | Result |
|---|---|
| `docker restart billing` | Same `agent_id`, therefore the same backend server/agent record. |
| Remove and recreate with `phelix-billing-data` | Same `agent_id`, therefore the same backend server/agent record. |
| Recreate without the Phelix data volume | A new `agent_id` and a new backend server/agent record. |

The backend registers an agent idempotently: an existing `agent_id` updates its
record and last-seen data; a new `agent_id` creates one. A reconnect must never
create another server merely because the container restarted.

### Application identity

`phelix build billing` (and equivalent application-creation flows) generates
an `app_id` once and persists it in `/var/lib/phelix/apps.json`. The
application may be related to an agent/server, but its ID must not be derived
from `server_id`, `agent_id`, or its name. The backend may enforce unique names
within an agent using `UNIQUE(server_id, name)`; names are not globally unique.

See [the backend identity migration guide](docs/backend-agent-identity-migration.md) for
the registration contract, schema, and rollout requirements. Local installs
(no Docker) store the same files under `~/.phelix`; the root may be overridden
with `PHELIX_DATA_DIR`.

### Matrix builds

Build the cross-product of **toolchain version × platform** in one command.
The matrix can be configured three ways — CLI flags, a `matrix:` profile in
`phelix.yaml`, or the interactive wizard — all three converge into the same
configuration and are validated identically.

```bash
# Native binaries
phelix build myapp --matrix --go-versions 1.22,1.23,1.27 --platforms linux/amd64,linux/arm64
phelix build myapp --matrix --rust-versions 1.77,1.78 --platforms linux/amd64,linux/arm64
phelix build myapp --matrix --go-versions 1.27 --platforms linux/arm/v7,linux/arm64

# Dry run / debug
phelix build myapp --matrix --go-versions 1.22,1.23 --platforms linux/amd64 --matrix-dry-run
phelix build myapp --matrix --go-versions 1.22,1.23 --platforms linux/amd64,linux/arm64 --debug

# Docker images (per-combination tags, optional multi-arch manifest)
phelix dockerize myapp --matrix --go-versions 1.22,1.27 --platforms linux/amd64,linux/arm64
phelix dockerize myapp --matrix --go-versions 1.27 --platforms linux/amd64,linux/arm64 --multi-arch-tag --push
phelix dockerize myapp --matrix --go-versions 1.22,1.27 --platforms linux/amd64,linux/arm64 --push --push-partial
```

| Flag | Description |
|------|-------------|
| `--matrix` | Enable matrix mode (auto-enabled when `--go-versions`/`--rust-versions`/`--platforms` are set, or when the phelix.yaml matrix profile is enabled; an explicit `--matrix=false` disables an enabled profile — combining it with dimension flags is rejected) |
| `--go-versions` | Comma-separated Go versions (`1.21`, `1.22.4`, `go1.27`, `v1.27` all work; patch versions allowed) |
| `--rust-versions` | Comma-separated Rust versions (same format) |
| `--platforms` | Target platforms (e.g. `linux/amd64,linux/arm64,linux/arm/v7,darwin/arm64`; case-insensitive) |
| `--matrix-concurrency` | Max parallel builds (default: 3) |
| `--matrix-retries` | Retry failed combinations up to N *additional* times (default: 0; only transient failures — network, timeout, Docker daemon — are retried) |
| `--resume` | Resume an interrupted matrix run (`--resume` picks the most recent, `--resume=mx_…` a specific run; implies matrix mode) |
| `--matrix-dry-run` | Print the matrix plan without executing |
| `--debug` | Verbose output: Docker commands, build logs, cache paths |
| `--multi-arch-tag` | (dockerize) additionally assemble a multi-arch manifest list per toolchain version via `docker buildx` |
| `--push-partial` | (dockerize) push only the successful images even if some combinations failed |

Versions are validated as `major.minor` or `major.minor.patch` — any current or
future toolchain release works, including patch versions. Known platforms:
`linux/{amd64,arm64,arm/v7,arm/v6}`, `darwin/{amd64,arm64}`, `windows/amd64`.
Duplicates are removed, whitespace and version prefixes (`go`, `rust`, `v`) are
normalized, and every combination is validated before the first build starts.

#### Matrix profile in phelix.yaml

The same matrix can be configured persistently in `phelix.yaml`:

```yaml
matrix:
  enabled: true
  go:                # or rust: — exactly one ecosystem
    versions:
      - "1.25"
      - "1.26"
      - "1.27"
  platforms:
    - linux/amd64
    - linux/arm64
    - windows/amd64
  concurrency: 4     # optional, default 3
  retries: 2         # optional, automatic retries for transient failures
  include:           # optional, extra combinations / metadata
    - go: "1.28"
      platform: linux/amd64
      tag: latest
  exclude:           # optional, drop matching combinations
    - go: "1.25"
      platform: windows/amd64
```

With `matrix.enabled: true`, a plain `phelix build` runs the matrix — no flags
needed — and so does a plain `phelix dockerize` (its Linux combinations build
images; other platforms fail fast with a pointer to `phelix build --matrix`).
The profile goes through the exact same validation and expansion as
CLI flags.

#### Matrix dimensions, include, and exclude

Internally the matrix is a set of **dimensions** (`lang`, `version`, `os`,
`arch`, `variant`); the Cartesian product of the configured versions ×
platforms is only the *base* of the expansion. The full pipeline is
deterministic and identical for CLI flags, `phelix.yaml`, the wizard, and any
future remote execution:

```text
Base Cartesian product  →  Include rules  →  Exclude rules  →  Final combinations
```

**Exclude** rules are partial matchers: a rule matches every combination that
carries the constrained values, regardless of the other dimensions. Rule keys:
`go`/`rust` (shorthand for ecosystem + version), `lang`, `version`,
`platform`, `os`, `arch`, `variant`. So this drops *all* Windows combinations
of Go 1.25 and nothing else:

```yaml
exclude:
  - go: "1.25"
    platform: windows/amd64
```

**Include** rules do two things:
- a rule that names a full combination (ecosystem + version + platform)
  **adds it** when the base product doesn't contain it — `go: "1.28"` above
  builds 1.28 even though only 1.25–1.27 are configured;
- any other keys on an include entry (like `tag: latest`) are **metadata**
  merged into the matching combination(s) — recorded in the run and visible
  in `matrix show`.

A duplicate include (a combination already in the matrix) never schedules the
job twice — it only merges its metadata. A well-formed rule that matches no
combination at the point it is applied is a **configuration error**, so a typo
cannot silently shrink (or fail to shrink) the matrix. Exclude rules reject
unknown keys outright for the same reason. Excludes apply *after* includes,
so an exclude can remove an included combination.

The wizard (`phelix matrix init`) configures includes and excludes too, and
its preview shows the real pipeline counts (`base 6 · included +1 ·
excluded -1`) computed by the same expansion engine the build uses.

**Configuration precedence** (per dimension, deterministic):

```text
CLI explicit value  >  phelix.yaml matrix profile  >  command default
```

- List dimensions are **replaced, never merged**: `phelix build --go-versions 1.28`
  next to the profile above builds only `1.28` (the YAML version list is
  overridden entirely), while unmentioned dimensions keep their configured
  values (platforms and concurrency above stay from the profile).
- `--matrix=false` explicitly disables an enabled profile; `--matrix` alone
  uses the configured profile when one exists.
- Versions and platforms are validated identically wherever they come from:
  an invalid profile fails `phelix build` (and every command that reads
  `phelix.yaml`) with an error naming the YAML location, e.g.
  `configuration error: matrix.go.versions: ...`.

#### Matrix runs: list, show, status, and the wizard

Every matrix execution gets a **Matrix Run ID** (`mx_20260909_8f31`) — printed
during the build, recorded in `builds/matrix/report.json` (`run_id`), attached
to each artifact in `versions.json` (`matrix_run_id`), and persisted locally
under `~/.phelix/matrix/runs/`. The run snapshots the effective configuration
at execution time, so later `phelix.yaml` edits never rewrite what an old run
says it built.

```bash
phelix matrix list                 # recorded runs, newest first (--limit N caps the list; default 20, 0 = all)
phelix matrix list --json          # machine-readable summaries
phelix matrix show mx_20260909_8f31        # config snapshot + per-combination results
phelix matrix show mx_20260909_8f31 --json
phelix matrix status               # live state of the active run (or "No active Matrix Run.")
phelix matrix status mx_20260909_8f31      # a specific run's state
phelix matrix status --json        # machine-readable current state
phelix matrix retry mx_20260909_8f31 --failed   # retry a run's failures in a new linked run
phelix matrix init                 # interactive wizard → writes phelix.yaml
```

`matrix show` displays the configuration snapshot (with each dimension's
origin: `cli`, `phelix.yaml`, or `default`, plus the effective include/exclude
rules and retry budget), every combination's status, duration, attempt
history, artifact, SHA-256, and redacted error details for failures. An
unknown run ID is a
clean `NOT_FOUND` error. Local run history can be relocated with
`PHELIX_DATA_DIR` like all Phelix state.

Run statuses: `succeeded` (everything passed), `partial` (some passed, some
failed), `failed` (nothing passed), `interrupted` (stopped with incomplete
combinations — resumable). A run with failures never reports `succeeded`.

#### Automatic retries

```bash
phelix build myapp --matrix --matrix-retries 2
```

`--matrix-retries 2` means **two additional attempts** (at most 3 executions
per combination), not two total. Only failed combinations are retried — a
combination that succeeded is never re-executed. Retries happen inside the
*same* Matrix Run: the run records every attempt (`Attempts: 3, final:
succeeded`) in `matrix show` and `report.json`, so an eventual success is
explainable. Only *transient* failures are retried (network, connection,
timeout, Docker daemon unavailable); deterministic failures — compiler
errors, invalid versions, configuration problems — fail on the first attempt
and are not repeated.

#### Resume

```bash
phelix build myapp --matrix --resume            # most recent interrupted run
phelix build myapp --matrix --resume=mx_20260909_8f31
```

Resume continues an **existing** incomplete run instead of starting a new
matrix. Runs persist incrementally (after every completed combination), so
resume works after Ctrl-C, a crash, or a machine restart. Exact semantics:

- `succeeded` combinations → **never rebuilt**
- `failed` combinations → skipped (retry them explicitly with
  `phelix matrix retry <run-id> --failed`)
- `pending` combinations and `running` combinations whose worker is gone →
  **executed**

Resume uses the run's original configuration snapshot (dimensions, include/
exclude rules, build args) — later `phelix.yaml` edits cannot change what the
resumed run builds. Explicit `--build-arg` flags on the resume invocation are
the one exception: they replace the snapshot's build args (and the run record
reflects the args actually used). A per-run lock file guards against two
concurrent executions of the same run; a lock left by a dead process is
reclaimed automatically after a restart. The first Ctrl-C stops the run cleanly
(in-flight builds are killed and stay pending); a second one terminates
immediately. `--matrix-dry-run` alongside `--resume` previews the combinations
a resume would execute without touching the run.

#### Manual retry

```bash
phelix matrix retry mx_20260909_8f31 --failed
```

A manual retry **creates a new run** containing only the combinations whose
final status in the source run is `failed`. The new run records its parent
(`parent_run_id`), executes the source run's configuration snapshot, and the
original run's history stays unchanged. The three recovery mechanisms are
deliberately distinct:

| Mechanism | Same run? | What executes |
|-----------|-----------|---------------|
| Automatic retry (`--matrix-retries`) | yes | failed combinations, extra attempts |
| Resume (`--resume`) | yes | incomplete combinations only |
| Manual retry (`matrix retry --failed`) | new linked run | failed combinations of the source run |

**Interactive wizard:** `phelix matrix init` walks through ecosystem, versions
(recent suggestions plus free-form entry), platforms, concurrency, optional
include entries (combinations outside the base matrix) and exclusions
(picked from the real expanded list), then previews the *actual* expanded
combination list with base/included/excluded counts — never a naive
versions × platforms count — before writing the profile. It preserves all
unrelated `phelix.yaml` keys and comments, offers Edit / Keep / Disable /
Cancel when a profile already exists, and refuses to run without an
interactive terminal (so it never hangs in CI).

#### Matrix status: live run state

```bash
phelix matrix status                       # the active run, or "No active Matrix Run."
phelix matrix status mx_20260909_8f31      # one specific run
phelix matrix status --json                # machine-readable current state
```

`matrix status` answers *"what is happening right now?"* — unlike `matrix
list` (all recorded runs) and `matrix show` (full inspection of one run). With
no argument it selects the **active run**: a run whose persisted status is
`running` *and* whose execution lock is held by a live process. When several
runs execute concurrently, the most recently started one is shown (with a
note); name a run explicitly to inspect another. If nothing is executing it
prints `No active Matrix Run.` and points at the most recent run — never a
fabricated status.

```text
Matrix Run mx_20260910_4613 — myapp (go)
Status:      running — executing (PID 3372540)
Started:     2026-09-10 01:49:03

4 combination(s)
──────────────────────────────────────────────
  ✓ go1.27-linux-amd64                  231ms
      Binary: 5621907 B
      SHA256: 3263d01d02d8…
  ⟳ go1.27-darwin-arm64                 7.2s
  ◌ go1.27-linux-arm64                  pending
  ◌ go1.27-windows-amd64                pending
──────────────────────────────────────────────
Progress: 1/4 — success 1 · failed 0 · running 1 · pending 2
```

Semantics:

- **Live state** comes from the same persisted run records the execution
  engine writes — after every attempt start and every completed combination.
  There is no second status system that could drift.
- `--json` always emits JSON: `{"active": false}` (plus `most_recent_run`
  when any history exists) when nothing is executing — never human text on
  the JSON stream.
- **Running combinations** show elapsed time (duration only — the build
  engine has no percentage to report) and the in-flight attempt when
  automatic retries are configured (`attempt 2/3`). Completed combinations
  show their final attempt count.
- **Counters are always internally consistent**: `success + failed + running
  + pending + skipped = total`.
- A persisted `running` run whose executing process is gone (crash, SIGKILL,
  machine restart) is reported as **orphaned** — resumable, not executing;
  its in-flight combinations show as `stale`. A completed run never reports
  `running`.
- Resume keeps the same run ID traceable (`Resumed: N time(s)`); a manual
  retry stays a **distinct** run linked via `Parent Run`. While a retry run
  executes, it is the active run.

#### Artifact checksums and the release manifest

Every successful matrix artifact carries a **SHA-256 checksum** computed from
the final artifact bytes — streamed, never loaded into memory wholesale. For
Docker image artifacts (matrix dockerize), the checksum is the image's
content digest (`docker image inspect`), Docker's own SHA-256 of the image.

Checksums appear:

- in the build summary and `matrix show` / `matrix status` (shortened, e.g.
  `SHA256: 3263d01d02d8…`),
- in full, machine-readably, in `builds/matrix/report.json` (`sha256` per
  combination), `matrix status --json`, each artifact row in `versions.json`
  (`sha256`), and the release manifest below.

A checksum that cannot be computed is an **artifact-integrity failure**: the
combination is recorded as failed — an artifact whose bytes cannot be
verified is never recorded as a valid release artifact (and integrity
failures are never auto-retried). Retries and resume never reuse stale
checksums: only the final successful attempt's artifact is checksummed and
recorded; resumed runs keep the checksums of previously-succeeded
combinations untouched.

> SHA-256 here is an **integrity** mechanism — it answers "did the artifact
> bytes change?". It does **not** provide authenticity ("who produced this
> artifact?"); Phelix has no artifact signing.

**One logical release, many artifacts.** A matrix build is one logical
build/release of one application version that produces multiple artifacts.
Matrix combinations are **artifacts of that release, never independent
application versions**: `go1.27-linux-amd64`, `go1.27-linux-arm64`, … are
artifact names (and filenames), while `versions.json` records exactly **one**
version row (`vN`) carrying all of them:

```text
Application
    └── Version vN (one versions.json row)
            └── Matrix Run mx_…
                    └── Artifacts: go1.27-linux-amd64, go1.27-linux-arm64, …
```

Each finished run with successful artifacts also writes a **release
manifest** describing that artifact set, stored next to the run record as
`<PHELIX_DATA_DIR>/matrix/runs/<run-id>.manifest.json`:

```json
{
  "manifest_version": 1,
  "app": "myapp",
  "version": 11,
  "matrix_run_id": "mx_20260910_3915",
  "created_at": "2026-09-10T01:46:38+03:30",
  "status": "complete",
  "total_combinations": 4,
  "language": "go",
  "toolchain_versions": ["1.27"],
  "platforms": ["linux/amd64", "darwin/arm64", "linux/arm64", "windows/amd64"],
  "artifacts": [
    {
      "combination_id": "go1.27-linux-amd64",
      "identity": "mx_20260910_3915/go1.27-linux-amd64",
      "toolchain": "go",
      "toolchain_version": "1.27",
      "platform": "linux/amd64",
      "os": "linux",
      "arch": "amd64",
      "artifact": "builds/matrix/go1.27-linux-amd64/myapp_amd64_go_1.27",
      "size_bytes": 2433430,
      "sha256": "dfef784b056f…"
    }
  ]
}
```

Completeness is explicit and fail-closed:

| Run outcome | Manifest |
|-------------|----------|
| all combinations succeeded | written, `status: "complete"` |
| some succeeded, some failed | written, `status: "partial"` (only successful artifacts listed) |
| nothing succeeded / interrupted / still running | **not written** |

A partial matrix can therefore never masquerade as a complete release — the
`status` field and the `total_combinations` count make the gap machine-checkable.
Artifacts are sorted by combination ID, so the same run and artifact set
always produce the same manifest. `matrix show <run-id>` renders the release
summary (version, status, artifact count, manifest path). An interrupted run
gains its manifest only once a `--resume` finishes it; a manual retry run
gets its own manifest — runs are never merged. Docker matrix builds
(`phelix dockerize --matrix`) record image digests per artifact in
`versions.json` but produce no run manifest (they are not Matrix Runs).

#### How matrix builds work

**Cross-compilation strategy:**
- **Go**: native cross-compilation via `GOOS`/`GOARCH` (plus `GOARM=6`/`7` for the ARM variant platforms) with `CGO_ENABLED=0` — no extra toolchain needed. CGO projects fail with a clear error suggesting Docker-based builds or a C cross-compiler. If no host Go toolchain is installed — or the host toolchain is a different Go version than the requested one — the build automatically falls back to the Docker path below, so an artifact is never labeled with a toolchain it was not built with.
- **Rust**: uses the [`cross`](https://github.com/cross-rs/cross) tool (not raw `rustup target add`), building inside a Docker container with the correct linker/C libraries pre-configured. The requested version is pinned via `cross +<version>`, so each combination really builds with its own toolchain (rustup auto-installs missing ones).

**Multi-version builds:** with more than one Go version (or no host Go
toolchain), each version runs inside its own Docker container (`golang:1.22`,
`golang:1.22.4`, …) — no need for multiple toolchains on the host, and clean
cache isolation. The build command is passed as separate arguments (never
through a shell), with `GOOS`/`GOARCH`/`GOARM`/`CGO_ENABLED` injected via
`docker run -e`.

**Matrix builds don't need a port:** they compile artifacts for other platforms
and never start a local instance, so port validation and availability checks are
skipped — a busy default port cannot block a cross-compilation run.

**Output naming** (every combination produces a unique, path-safe artifact —
combinations can never overwrite each other):

| Artifact | Naming |
|----------|--------|
| Binary | `builds/matrix/{lang}{version}-{os}-{arch}[-{variant}]/{app}_{arch}_{lang}_{version}[_{variant}]` — e.g. `builds/matrix/go1.22.4-linux-arm-v7/myapp_arm_go_1.22.4_v7` |
| Docker per-combination tag | `{app}:{tag\|latest}-{lang}{version}-{arch}[-{variant}]` — e.g. `myapp:v1.2.3-go1.22-amd64`, `myapp:latest-go1.22.4-arm-v7` |
| Docker multi-arch manifest | `{app}:{tag\|latest}` for a single combination, version-qualified (`{app}:{tag}-{version}`) whenever the run produced more than one combination (so concurrent versions never overwrite each other's manifest) |
| JSON report | `builds/matrix/report.json` |
| Release manifest | `<PHELIX_DATA_DIR>/matrix/runs/<run-id>.manifest.json` (one per finished run with artifacts; see [Artifact checksums and the release manifest](#artifact-checksums-and-the-release-manifest)) |

**Docker matrix builds are linux-only:** Docker images cannot target
`darwin/*` or `windows/*`, so those combinations fail fast with a pointer to
`phelix build --matrix` for native binaries. Per-combination `TARGET*` build
args (`TARGETPLATFORM`, `TARGETOS`, `TARGETARCH`, `TARGETVARIANT`,
`TARGETVERSION`) are passed to every image build, so generated Dockerfiles can
react to the target, and the requested toolchain version is additionally
passed as `GO_VERSION`/`RUST_VERSION` — the build args the generated
Dockerfiles pin their toolchain images on (see the `ARG GO_VERSION`/
`ARG RUST_VERSION` notes in
[Docker image building](#docker-image-building)). A Dockerfile is generated
when the project has none, exactly like a single-image dockerize. Your own
`--build-arg KEY=VALUE` flags are forwarded too, and malformed entries (`KEY`
without a value, empty keys) are rejected up front instead of being silently
dropped.

> **Note:** `--matrix-tags` is kept for compatibility but is a no-op —
> per-combination tagging is the default behavior of every matrix dockerize.

**Concurrency and caching:** worker pool runs combinations in parallel (default: 3); each combination gets its own cache directory (`.phelix/cache/go/{combo-id}/` or `target/{combo-id}/`); live progress display shows running/completed/failed combinations. A combination whose build panics or returns no result is recorded as failed and never takes down the rest of the matrix.

**Failure handling:**
- **Build phase**: fail-open — one failure doesn't stop the rest; full summary at the end. The CLI exits non-zero when any combination failed, so scripts can detect partial failures.
- **Push phase**: fail-closed by default — if any combination failed, nothing is pushed. Use `--push-partial` to push only successful images.

**Reporting:** a terminal summary plus a JSON report (`builds/matrix/report.json`, carrying the Matrix Run ID as `run_id`) listing each combination's status, duration, artifact path, SHA-256 checksum, cache status, attempt count and per-attempt log (automatic retries), and error (if failed — redacted, so compiler output with embedded credentials never lands in the report). The report describes the whole run: a resumed run's report covers every combination (earlier sessions included), and combinations that never ran appear as `pending`.

**Independent build reports per combination:** every combination retains its own build metrics — toolchain version, target platform, duration, cache status and binary size — recorded alongside the matrix version's artifacts in `versions.json` and mirrored in `builds/matrix/report.json` (`cache_status` per combination). Regression analysis is combination-aware: `Go 1.27 / linux-amd64` is only ever compared against previous `Go 1.27 / linux-amd64` builds, never against `Go 1.26 / linux-arm64` or a Rust build. After recording, each combination prints a compact summary of its own comparisons.

### Other commands

```bash
phelix version [--short | --verbose]
phelix update [--check]   # update the Phelix binary to the latest release
phelix monitor      # start the long-running gRPC monitoring daemon (foreground)
```

## Multi-Server Monitoring

#### `phelix monitor`
Starts the long-running gRPC monitoring daemon that:
- Restores managed applications that were previously running (auto-start apps)
- Opens a single, long-lived, TLS-secured gRPC stream to the Phelix backend (requires an authenticated session — see [Authentication](#authentication))
- Monitors application status across all servers
- Sends application information, resource metrics, and logs to the central server roughly every 2 seconds
- Automatically reconnects with exponential backoff if the connection is lost
- Provides real-time updates for all managed applications

It runs in the **foreground** and stays attached to the terminal when invoked
manually. On Linux it is normally started by the systemd unit created by
`setup.sh`/`install.sh`, which supervises `phelix monitor` directly
(`/usr/local/bin/phelix monitor` as `ExecStart`) — there is no backgrounding
shell wrapper. Manage it with:

```bash
sudo systemctl start phelix
sudo systemctl restart phelix
sudo systemctl stop phelix
sudo systemctl status phelix
sudo journalctl -u phelix -f
```

#### Server management
Phelix can monitor multiple servers simultaneously. Each server running Phelix will:
- Register itself with the central monitoring service
- Send regular status updates
- Maintain its own application state
- Sync with other servers when needed

## System Requirements

- Go and/or Rust toolchain (auto-installed on Linux if missing; manual install required on other OSes)
- Internet connection for authentication and monitoring (optional — only needed for the `phelix.anophel.com` dashboard)
- Sufficient permissions to create and manage application files
- Network access between servers (if monitoring multiple servers)
- Docker (only if using `phelix dockerize`)

## Where Phelix Stores Data

All state lives under `~/.phelix/`:

```
~/.phelix/
├── apps.json              # registry of all managed apps
├── master.key              # AES-256-GCM master key for env encryption (0600)
├── session.json            # auth session
├── config.json             # server configuration
├── proxy.sock               # proxy daemon control socket
├── logs/
│   ├── phelix.log           # Phelix's own log
│   ├── <app>.log            # per-app logs
│   └── deploy_*.log         # deploy instance logs
├── registry/<slug>.enc      # encrypted registry credentials
├── matrix/
│   └── runs/                 # Matrix Run history: <id>.json records,
│                             #   <id>.lock execution locks, <id>.manifest.json
│                             #   release manifests (see Matrix builds)
└── apps/<AppName>/          # per-app data
    ├── versions.json        # version metadata index (incl. per-version build reports + per-combo matrix reports)
    ├── deploy.json           # blue-green / rolling state
    ├── rollback.log          # rollback audit trail
    ├── current → builds/vN   # symlink to the active build
    ├── builds/vN/binary       # versioned binaries
    └── env/vN.enc              # per-version encrypted env snapshot
```

Retention: the last **5** versions are kept by default (configurable per plan); the active version is never pruned. Build-report metadata lives inside `versions.json` (the `build_report` field per version) — there is no separate build database, and everything works offline.

## Error Handling

- Errors are rendered **once, on stderr**, with a short code, the message, and
  an actionable hint. Successful output stays on stdout and is never polluted
  with error text.
- Authentication errors (e.g. an expired session during `phelix auth status`)
  prompt you to run `phelix auth login`. Build/run commands never require it.
- Build errors show the failing stage and a hint, and the wrapped root-cause
  chain (e.g. the failing cargo/go command with its exit status and the useful
  tail of its diagnostics) is rendered on stderr — no `--debug` required.
- Connection errors are logged and retried automatically.
- Server communication errors are handled gracefully, without crashing the CLI.
- **Root causes are preserved.** Wrapped errors keep the underlying cause
  reachable via `errors.Is` / `errors.As`, so `os.IsNotExist`, `exec.ExitError`,
  and gRPC `status.Code` still work on the cause.
- **`--debug`**: passes the flag to any command to render the complete error
  chain on stderr (normal mode bounds the chain to the first few wrapped
  layers; `--debug` lifts that cap). It never discloses secrets — every rendered
  string is run through `phelixerr.Redact` in both normal and debug mode.

### Error Reporter (known errors)

For a small registry of **known, recognizable problems**, Phelix adds an
explanation, a concrete suggested fix, a real command you can run yourself,
and a documentation link on top of the usual error line. The error code, exit
code, and root-cause chain are unchanged — the reporter only adds context.

Currently recognized:

| Problem | Example guidance |
|---|---|
| Missing **Go** toolchain (`TOOLCHAIN_NOT_FOUND`) | platform-appropriate install command (via Phelix's package-manager detection: `sudo apt-get …` / `sudo dnf …` / `brew install go` / `winget …`), docs at go.dev/doc/install |
| Missing **Rust** toolchain (`TOOLCHAIN_NOT_FOUND`) | rustup install command, docs at rust-lang.org/tools/install |
| **Port already in use** (`PORT_UNAVAILABLE`) | names the listening process and PID when the OS can tell (read-only `lsof` lookup), a read-only inspection command, and the `--port` alternative. Phelix **never** stops the process for you |
| **go.mod problems** (`BUILD_FAILED`) | `go mod init` for a missing go.mod, `go mod tidy` for go.sum drift or undeclared dependencies, manual-fix guidance for malformed go.mod, cache-clear guidance for checksum mismatches — each only for the specific condition it matches |

Example (`go.mod` with an unknown directive):

```text
Error: build failed
  Code: BUILD_FAILED

  Invalid go.mod

  go.mod could not be parsed — it likely contains a syntax error or an
  unsupported directive at the line named in the go tool output below.

  Suggested fix:
  Fix the reported line in go.mod, then build again. Once the file parses,
  go mod edit -fmt reformats it.
  Documentation:
    https://go.dev/ref/mod

  Tool output (most recent lines):
    go: errors parsing go.mod:
    go.mod:5: unknown directive: toolchainx
```

**Unknown errors stay unknown.** Most compiler and system failures have no
smart suggestion — and none is invented for them. An unrecognized error keeps
its code, shows the raw captured tool output (if any) and the root cause via
`--debug`, exactly as before. Only deterministic, evidence-based signals
(structured error codes, `errors.Is`/`errors.As`, tool identity, and narrowly
matched, tested tool-output conditions) trigger a suggestion; generic words
like "error" or "failed" never do.

Suggested commands are **informational only** — Phelix never executes them for
you, and every rendered string (including captured compiler output) passes
through the same secret redactor as the rest of the error path.

### Exit codes

Exit codes are part of the CLI's script-facing contract: **same category ⇒ same
exit code**, so automation can rely on them:

| Exit | Category | Typical codes |
|-----:|----------|---------------|
| 0 | success | — |
| 1 | generic failure | `UNKNOWN`, `SERVER_ERROR`, `ENCRYPTION_ERROR`, plain errors |
| 2 | invalid usage / arguments | `INVALID_ARGUMENT`, `VALIDATION` |
| 10 | authentication | `UNAUTHENTICATED`, `INVALID_CREDENTIALS`, `SESSION_EXPIRED` |
| 11 | permission | `PERMISSION_DENIED` |
| 12 | not found | `NOT_FOUND`, `VERSION_NOT_FOUND`, `ROLLBACK_TARGET_NOT_FOUND` |
| 20 | build | `BUILD_FAILED`, `BUILD_TIMEOUT`, `TOOLCHAIN_NOT_FOUND`, `UNSUPPORTED_PROJECT` |
| 21 | deploy | `DEPLOY_FAILED`, `INSTANCE_START_FAILED`, `HEALTH_CHECK_FAILED`, `DEPLOY_LOCKED` |
| 22 | rollback | `ROLLBACK_FAILED` |
| 23 | rollback verification failed/cancelled | `ROLLBACK_VERIFY_FAILED` |
| 24 | deployment failed AND automatic rollback failed (state degraded — manual intervention required) | `AUTO_ROLLBACK_FAILED` |
| 30 | network | `CONNECTION_ERROR`, `PORT_UNAVAILABLE` |
| 40 | configuration | `CONFIGURATION_ERROR` |
| 50 | docker | `DOCKER_ERROR` |
| 60 | timeout | `TIMEOUT` |
| 70 | encryption | `ENCRYPTION_ERROR` |

Examples: `phelix status no-such-app` exits **12** (`NOT_FOUND`);
`phelix status` (missing required argument) exits **2**. See
[`docs/error-architecture.md`](docs/error-architecture.md) for the full error
architecture, code inventory, the error-reporter registry, and developer
guidelines.

## Best Practices

1. Log in (`phelix auth login`) to push metrics, logs, and events to your dashboard; build/run works fine without it.
2. Use meaningful, unique names for your applications.
3. Monitor application logs (`phelix log`) for debugging.
4. Use `phelix status` to check application health regularly.
5. Log in once and keep your session active — apps you create while logged out are synced to the dashboard the next time you log in.
6. Ensure proper network connectivity between servers.
7. Regularly check server status across your infrastructure.
8. Monitor resource usage (RAM/CPU) across all servers.
9. **Use encrypted environment variables for sensitive data** (API keys, database credentials, etc.).
10. **Never commit master keys or encrypted env files to version control.**
11. **Regularly rotate sensitive credentials.**
12. **Use descriptive variable names** (e.g., `DATABASE_CONNECTION_URL` instead of `DB`).
13. **Use `phelix rollback --list`** to review available versions before rolling back.
14. **Preview destructive rollbacks with `--dry-run`** before executing — especially for production rollbacks, large rollback distances (many versions behind), tagged-release rollbacks, rollbacks after a failed deployment, and blue-green/rolling apps where the traffic transition matters:
    ```bash
    phelix rollback myapp --to stable --dry-run   # inspect the plan
    phelix rollback myapp --to stable             # then execute
    ```
15. **Keep the proxy daemon running** (`phelix proxy`) for zero-downtime rollbacks.
16. **Use `--tag`** to label important builds (e.g. `--tag "v2.1-release"`) for easier rollback identification.
17. **Check `phelix status <app>`** for version history before deciding to roll back.
18. **Use `phelix dockerize`** to containerize apps with optimized, cached Dockerfiles.
19. **Watch the automatic Build Report after every build** — it is the fastest way to detect unexpected binary growth, compilation regressions, or toolchain changes (the compiler version in the report makes accidental toolchain bumps visible).
20. **Treat repeated duration regressions in the same cache mode as a signal**: if cold builds keep getting slower across versions, the codebase — not the cache — is the problem.
20. **Use `phelix build-report <AppName>`** to review stored build history before investigating a performance or size issue; it is read-only and works offline.
21. **After a matrix build, check each combination's summary** — a size regression in one platform/toolchain combination won't show up in the others.

## Security Considerations

- All communication with the Phelix service is encrypted (only when logged in — offline mode sends nothing).
- Authentication tokens are securely stored (`~/.phelix/session.json`).
- Server-to-server communication is authenticated.
- Sessions are regularly validated.
- Secure file permissions on sensitive files (`master.key` at `0600`).
- Environment variables and registry credentials are encrypted at rest with AES-256-GCM and never logged in plaintext.
- When not logged in, no app data, metrics, or events leave your machine.

## Support

- Documentation: [phelix.anophel.com/docs](https://phelix.anophel.com/docs)

object is licensed under the MIT License — see the `LICENSE` file for details.
