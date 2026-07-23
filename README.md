# Phelix

A CLI tool for building, running, deploying, and managing **Go** and **Rust** applications — with versioned builds, zero-downtime blue-green/rolling deploys, Docker image building, encrypted environment variables, health checks.

Phelix is written in Go and built with [Cobra](https://github.com/spf13/cobra). It targets Linux (with experimental Windows support).

---

## Table of Contents

- [Quick Start](#quick-start)
  - [Installation](#installation)
  - [Authentication](#authentication)
  - [Core Workflow](#core-workflow)
  - [Command Reference](#command-reference)
  - [Where Phelix Stores Data](#where-phelix-stores-data)

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
enables a `phelix.service` **systemd** unit that starts the background WebSocket
monitor and all managed apps on boot.

```bash
phelix version              # verify the install
sudo systemctl status phelix   # Linux: monitor service running?
```

### Alternative — build from source

```bash
git clone <repo-url> Phelix
cd Phelix
go build -o phelix
sudo mv phelix /usr/local/bin/
```

### Alternative — `go install`

```bash
go install github.com/abdorrahmani/phelix@latest
```

> The in-repo `setup.sh` is the developer-facing variant of the installer: it
> builds from source with `go build`, installs the binary, and creates the same
> systemd unit. The hosted `install.sh` is the end-user version that downloads a
> prebuilt binary per-OS instead of requiring a local Go toolchain.

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

Most commands require an authenticated session with the Phelix service
(`phelix.anophel.com`). Authenticate once; the session is stored locally at
`~/.phelix/session.json`.

```bash
# Interactive
phelix auth login

# Non-interactive
phelix auth login --username <user> --apiKey <key>

phelix auth status     # show current session + expiry
phelix auth logout     # invalidate and remove the session
```

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
The encryption key lives at `~/.phelix/master.key` (auto-generated).

```bash
phelix env set    MyApp DATABASE_URL=postgresql://localhost/db
phelix env get    MyApp DATABASE_URL      # sensitive values are masked
phelix env list   MyApp                   # values shown as ***REDACTED***
phelix env unset  MyApp DATABASE_URL
phelix env check  MyApp DATABASE_URL      # exit-status friendly existence check
```

Encrypted values are injected into the app process at start, and snapshotted
alongside each versioned build so a rollback restores the matching env.

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

```bash
phelix proxy            # start in background (detaches and returns)
phelix proxy status     # show enrolled apps + routing
phelix proxy stop       # shut the daemon down (drains up to 30s)
phelix proxy --foreground   # run attached (for process supervisors)
```

### Docker

#### `phelix dockerize <AppName> [flags]`
Generates a multi-stage `Dockerfile` (if absent — never overwrites), a
`.dockerignore`, builds the image, and records it as a version.

| Flag | Default | Description |
|------|---------|-------------|
| `--tag` | `latest` | Image tag (e.g. `v1.2.3`) |
| `--registry` | — | Registry prefix (e.g. `ghcr.io/user`) |
| `--push` | `false` | Push after building |
| `--build-arg, -a` | — | `KEY=value` build args (repeatable) |
| `--with-compose` | `false` | Generate `docker-compose.yml` |
| `--depends-on` | — | Sidecar services: `redis,postgres,mysql,mongodb,rabbitmq` |
| `--matrix` + family | — | Matrix Docker builds (see [Matrix builds](#matrix-builds)) |

```bash
phelix dockerize myapp --tag v1.2.3 --registry ghcr.io/me --push
phelix dockerize myapp --with-compose --depends-on redis,postgres
```

> Phelix generates the compose file but does **not** manage sidecar lifecycles —
> use `docker compose up -d` for those.

### Matrix builds

Build the cross-product of **toolchain version × platform** in one command.
Reports per-combination results and writes a JSON report.

```bash
# Native binaries
phelix build myapp --matrix --go-versions 1.22,1.23 --platforms linux/amd64,linux/arm64

# Docker images (multi-arch or per-combo tags)
phelix dockerize myapp --matrix --go-versions 1.22,1.23 \
    --platforms linux/amd64,linux/arm64 --multi-arch-tag --push
```

Known Go versions: `1.20`–`1.26`. Known Rust versions: `1.75`–`1.97` (`1.80,1.90,1.97`...).
Known platforms: `linux/{amd64,arm64,arm/v7,arm/v6}`, `darwin/{amd64,arm64}`, `windows/amd64`.

### Other commands

```bash
phelix version [--short | --verbose]
phelix monitor      # start the background WebSocket monitoring service
```

`phelix monitor` is the long-running process that reports app metrics back to
the Phelix service. It's normally started by the systemd unit created by
`setup.sh`, not run manually.

## Where Phelix Stores Data

All state lives under `~/.phelix/`:

```
~/.phelix/
├── apps.json            # registry of all managed apps
├── master.key           # AES-256-GCM master key for env encryption
├── session.json         # auth session
├── proxy.sock           # proxy daemon control socket
├── logs/
│   ├── phelix.log       # Phelix's own log
│   └── <app>.log        # per-app logs
└── apps/<AppName>/      # per-app data
    ├── versions.json    # version metadata index
    ├── deploy.json      # blue-green / rolling state
    ├── current → builds/vN   # symlink to the active build
    ├── builds/vN/binary      # versioned binaries
    └── env/vN.enc            # per-version encrypted env snapshot
```

Retention: the last **5** versions are kept (configurable); the active version is
never pruned.
