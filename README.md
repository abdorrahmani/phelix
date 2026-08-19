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
- **Versioned builds with zero-downtime rollback** (all builds create versioned artifacts; `--tag` for meaningful labels)
- **Docker image building** (auto-generated multi-stage Dockerfiles for Go/Rust with optimized layer caching)
- **Matrix builds** (build multiple compiler-version × platform combinations in one command)

---

## Table of Contents

- [Quick Start](#quick-start)
- [Installation](#installation)
- [Authentication (optional)](#authentication)
- [Core Workflow](#core-workflow)
- [Command Reference](#command-reference)
- [Multi-Server Monitoring](#multi-server-monitoring)
- [System Requirements](#system-requirements)
- [Where Phelix Stores Data](#where-phelix-stores-data)
- [Error Handling](#error-handling)
- [Best Practices](#best-practices)
- [Security Considerations](#security-considerations)
- [Support](#support)

## Quick Start

```bash
# Build and run an app from the current directory (auto-detects Go or Rust)
phelix build myapp --port 8080

# Rebuild after a code change — zero downtime via blue-green
phelix rebuild myapp --blue-green

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
4. **Roll back** to any retained version if something breaks.
5. Monitor with `status`, `list`, `log`, and `health`.
6. Ship containers with `dockerize`.

## Command Reference

### Building & rebuilding

#### `phelix build <NAME> [flags]`
Compiles the project in the current directory (auto-detects Go or Rust), starts
it on the given port, and records a **new versioned build**.

| Flag | Default | Description |
|------|---------|-------------|
| `--port, -p` | `8080` | Port to run the app on |
| `--build-arg, -a` | — | Extra args passed to the build tool (repeatable) |
| `--tag` | — | Human label stored with the version (e.g. `"hotfix-auth"`) |
| `--no-upload` | `false` | Don't sync app info to the server |
| `--debug` | `false` | Verbose build/tool output |
| `--matrix` | `false` | Matrix mode: build versions × platforms (see [Matrix builds](#matrix-builds)) |
| `--go-versions` | — | Go versions, e.g. `1.22,1.23` |
| `--rust-versions` | — | Rust versions, e.g. `1.77,1.78` |
| `--platforms` | — | Targets, e.g. `linux/amd64,linux/arm64` |
| `--matrix-concurrency` | 4 | Max parallel matrix builds |
| `--matrix-dry-run` | `false` | Print the plan without building |

```bash
phelix build myapp -p 8080 --tag "v1.2.3"
```

#### `phelix rebuild <ID|AppName> [flags]`
Rebuilds an existing app from its source directory. Supports zero-downtime
deploy.

| Flag | Default | Description |
|------|---------|-------------|
| `--port, -p` | previous/`8080` | Port to run on |
| `--build-arg, -a` | — | Extra build args (repeatable) |
| `--tag` | — | Version label |
| `--no-upload` | `false` | Skip server sync |
| `--blue-green` | `false` | Zero-downtime blue-green deploy (needs `phelix proxy`) |
| `--replicas` | `0` | Zero-downtime rolling deploy over N replicas |

```bash
phelix rebuild myapp --blue-green
phelix rebuild myapp --replicas 3
```

### Versioning & rollback

#### `phelix rollback <AppName> [flags]`
Reverts to a previous versioned build. Automatically chooses the right strategy:
classic stop→start for plain builds, or zero-downtime for apps deployed with
blue-green/rolling.

| Flag | Default | Description |
|------|---------|-------------|
| `--to` | — | Target: `v3`, `3`, or a tag name (default: previous version) |
| `--list` | `false` | List all retained versions with metadata |

```bash
phelix rollback myapp              # previous version
phelix rollback myapp --to v3
phelix rollback myapp --to "hotfix-auth"
phelix rollback myapp --list
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

```bash
phelix proxy              # start in background (detaches and returns)
phelix proxy status       # show enrolled apps + routing
phelix proxy stop         # shut the daemon down (drains up to 30s)
phelix proxy --foreground   # run attached (for process supervisors / systemd)
```
`phelix rebuild --blue-green` / `--replicas` will also auto-start the daemon if it is not already running.

#### Blue-green deploy
Builds a new binary, starts it on the inactive slot (blue ↔ green), waits until healthy, then atomically switches the proxy. The previous instance is drained and stopped.
```bash
phelix rebuild myapp --blue-green
phelix rebuild myapp --blue-green --port 8080
```

#### Rolling deploy
Restarts N replicas one at a time (never more than one down). Useful when you want capacity during the cut-over.
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
phelix rollback myapp              # roll back to the previous version
phelix rollback myapp --to v2      # roll back to a specific version
phelix rollback myapp --to 3       # version number without 'v' prefix also works
phelix rollback myapp --to hotfix-auth-bug   # roll back by tag name
```

The `--to` flag accepts either a version ID (`v3`, `3`) or a unique tag name. If a tag matches exactly one version, it resolves automatically. If a tag matches zero or more than one version, an error is returned — use a version ID to disambiguate.

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

#### How rollback works

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
| `-a, --build-arg` | Extra build argument `KEY=value` (repeatable) |
| `--with-compose` | Generate a `docker-compose.yml` with the app service |
| `--depends-on` | Sidecar services for compose (`redis`, `postgres`, `mysql`, `mongodb`, `rabbitmq`) |
| `--matrix` + family | Matrix Docker builds (see [Matrix builds](#matrix-builds)) |

#### How it works

**Language detection:** checks for `go.mod` (Go) or `Cargo.toml` (Rust); fails with a clear error if neither or both are found.

**Dockerfile generation (if none exists):** multi-stage builds optimized for Docker layer caching — dependency-heavy layers are cached separately from source code.

*Go:*
1. **Builder stage** — `COPY go.mod go.sum` → `go mod download` → `COPY . .` → `go build`. Dependencies are cached before source is copied; `CGO_ENABLED=0` for a fully static binary.
2. **Runtime stage** — `FROM scratch` with just the binary. Smallest possible image.

*Rust:*
1. **Dependency cache stage** — copies `Cargo.toml`/`Cargo.lock`, builds a dummy `main.rs` to compile and cache all dependencies.
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

### Matrix builds

Build the cross-product of **toolchain version × platform** in one command.

```bash
# Native binaries
phelix build myapp --matrix --go-versions 1.21,1.22,1.23 --platforms linux/amd64,linux/arm64
phelix build myapp --matrix --rust-versions 1.77,1.78 --platforms linux/amd64,linux/arm64

# Dry run / debug
phelix build myapp --matrix --go-versions 1.22,1.23 --platforms linux/amd64 --matrix-dry-run
phelix build myapp --matrix --go-versions 1.22,1.23 --platforms linux/amd64,linux/arm64 --debug

# Docker images (multi-arch or per-combo tags)
phelix dockerize myapp --matrix --go-versions 1.22,1.23 --platforms linux/amd64,linux/arm64 --matrix-tags
phelix dockerize myapp --matrix --go-versions 1.22,1.23 --platforms linux/amd64,linux/arm64 --multi-arch-tag --push
phelix dockerize myapp --matrix --go-versions 1.22,1.23 --platforms linux/amd64,linux/arm64 --push --push-partial
```

| Flag | Description |
|------|-------------|
| `--matrix` | Enable matrix mode (auto-enabled when `--go-versions`/`--rust-versions`/`--platforms` are set) |
| `--go-versions` | Comma-separated Go versions (e.g. `1.21,1.22,1.23`) |
| `--rust-versions` | Comma-separated Rust versions (e.g. `1.77,1.78`) |
| `--platforms` | Target platforms (e.g. `linux/amd64,linux/arm64,darwin/arm64`) |
| `--matrix-concurrency` | Max parallel builds (default: 3–4) |
| `--matrix-dry-run` | Print the matrix plan without executing |
| `--debug` | Verbose output: Docker commands, build logs, cache paths |

Known Go versions: `1.20`–`1.26`. Known Rust versions: `1.75`–`1.97`.
Known platforms: `linux/{amd64,arm64,arm/v7,arm/v6}`, `darwin/{amd64,arm64}`, `windows/amd64`.

#### How matrix builds work

**Cross-compilation strategy:**
- **Go**: native cross-compilation via `GOOS`/`GOARCH` (no extra toolchain needed with `CGO_ENABLED=0`). CGO projects fail with a clear error suggesting Docker-based builds or a C cross-compiler.
- **Rust**: uses the [`cross`](https://github.com/cross-rs/cross) tool (not raw `rustup target add`), building inside a Docker container with the correct linker/C libraries pre-configured.

**Multi-version builds:** each toolchain version runs inside its own Docker container (`golang:1.21`, `golang:1.22`, etc.) — no need for multiple toolchains on the host, and clean cache isolation.

**Output naming:**

| Artifact | Naming |
|----------|--------|
| Binary (matrix) | `builds/matrix/go1.22-linux-amd64/binary` |
| Docker per-combo tag | `myapp:go1.22-linux-amd64` |
| Docker multi-arch tag | `myapp:latest` (manifest list via `docker buildx`) |
| JSON report | `builds/matrix/report.json` |

**Concurrency and caching:** worker pool runs combinations in parallel (default: 3); each combination gets its own cache directory (`.phelix/cache/go/{combo-id}/` or `target/{combo-id}/`); live progress display shows running/completed/failed combinations.

**Failure handling:**
- **Build phase**: fail-open — one failure doesn't stop the rest; full summary at the end.
- **Push phase**: fail-closed by default — if any combination failed, nothing is pushed. Use `--push-partial` to push only successful images.

**Reporting:** a terminal summary plus a JSON report (`builds/matrix/report.json`) listing each combination's status, duration, artifact path, and error (if failed).

### Other commands

```bash
phelix version [--short | --verbose]
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
└── apps/<AppName>/          # per-app data
    ├── versions.json        # version metadata index
    ├── deploy.json           # blue-green / rolling state
    ├── rollback.log          # rollback audit trail
    ├── current → builds/vN   # symlink to the active build
    ├── builds/vN/binary       # versioned binaries
    └── env/vN.enc              # per-version encrypted env snapshot
```

Retention: the last **5** versions are kept by default (configurable per plan); the active version is never pruned.

## Error Handling

- Errors are rendered **once, on stderr**, with a short code, the message, and
  an actionable hint. Successful output stays on stdout and is never polluted
  with error text.
- Authentication errors (e.g. an expired session during `phelix auth status`)
  prompt you to run `phelix auth login`. Build/run commands never require it.
- Build errors show the failing stage and a hint (run with `--debug` for the
  underlying root cause).
- Connection errors are logged and retried automatically.
- Server communication errors are handled gracefully, without crashing the CLI.
- **Root causes are preserved.** Wrapped errors keep the underlying cause
  reachable via `errors.Is` / `errors.As`, so `os.IsNotExist`, `exec.ExitError`,
  and gRPC `status.Code` still work on the cause.
- **`--debug`**: passes the flag to any command to render the full error chain
  (root cause included) on stderr. It never discloses secrets — every rendered
  string is run through `phelixerr.Redact` in both normal and debug mode.

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
| 30 | network | `CONNECTION_ERROR`, `PORT_UNAVAILABLE` |
| 40 | configuration | `CONFIGURATION_ERROR` |
| 50 | docker | `DOCKER_ERROR` |
| 60 | timeout | `TIMEOUT` |
| 70 | encryption | `ENCRYPTION_ERROR` |

Examples: `phelix status no-such-app` exits **12** (`NOT_FOUND`);
`phelix status` (missing required argument) exits **2**. See
[`docs/error-architecture.md`](docs/error-architecture.md) for the full error
architecture, code inventory, and developer guidelines.

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
14. **Keep the proxy daemon running** (`phelix proxy`) for zero-downtime rollbacks.
15. **Use `--tag`** to label important builds (e.g. `--tag "v2.1-release"`) for easier rollback identification.
16. **Check `phelix status <app>`** for version history before deciding to roll back.
17. **Use `phelix dockerize`** to containerize apps with optimized, cached Dockerfiles.

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