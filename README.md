# Phelix

A single-binary CLI for building, running, versioning, and **zero-downtime
deploying Go and Rust applications** on a host — with encrypted environment
variables, health checks, Docker workflows, and optional multi-server monitoring
through a dashboard at `phelix.anophel.com`.

Phelix is written in Go and built with [Cobra](https://github.com/spf13/cobra).
It targets Linux, with experimental Windows support.

## Why Phelix

Shipping a change to a small fleet usually means stitching together a build step,
a process manager, a reverse proxy, a rollback plan, and some way to watch it all.
Phelix folds that into one binary you run on the host: it compiles your app,
records every build as a numbered version, and cuts traffic over to the new
version only after it passes a health check — so a broken deploy never takes down
the running one. When something is wrong, you roll back to any retained version.

It is **offline-first**: build, run, deploy, and roll back work with no account
and no network. Logging in only adds dashboard sync — nothing leaves your machine
until you opt in, per application.

## Features

- **Go & Rust** application lifecycle — build, run, rebuild, start/stop/restart,
  status, logs.
- **Versioned builds** — every build is `v1`, `v2`, … with optional `--tag`;
  binary + encrypted env are snapshotted together.
- **Zero-downtime deploys** — blue-green and rolling via the `phelix proxy`.
- **Canary & progressive rollouts** — shift a traffic percentage, verify health
  and metrics against the stable baseline, auto-rollback on regression.
- **Rollback** — interactive picker or `--to`, dry-run preview, verification
  window, automatic rollback on failed deploy, audited history.
- **Health checks** — tiered deploy health gating (2xx / any-HTTP / TCP / PID).
- **Docker** — generate optimized multi-stage images (`dockerize`), or run app
  **instances as containers** under the proxy.
- **Matrix builds** — toolchain-version × platform cross-product, with resume,
  retries, per-artifact SHA-256, and release manifests.
- **Build reports** — automatic per-build metrics and regression alerts, offline.
- **Encrypted env vars** — AES-256-GCM at rest, injected at start, versioned.
- **Per-instance resource limits** — CPU/memory via cgroups v2 (Linux).
- **Git webhook deploys** — HMAC-authenticated push webhooks deploy the exact
  pushed commit through the normal rebuild pipeline.
- **Monitoring** — optional, per-app opt-in gRPC stream to the dashboard.

See the [documentation index](#documentation) for details on any of these.

## Quick Start

Install (detects OS/arch, no toolchain needed just to install):

```bash
curl -fsSL https://phelix.anophel.com/install.sh | bash
phelix version
```

Then, from a Go or Rust project directory:

```bash
# Initialize project config (creates phelix.yaml)
phelix init

# Build and run an app (auto-detects Go or Rust)
phelix build myapp --port 8080

# Rebuild after a change — zero downtime via blue-green
phelix rebuild myapp --blue-green

# Roll back to the previous versioned build
phelix rollback myapp
```

> **One requirement:** managed apps must listen on the port given by the `PORT`
> environment variable rather than hardcoding one — see
> [The PORT Contract](docs/getting-started/the-port-contract.md). Run
> `phelix doctor` to check.

Full walk-through: [Quick Start](docs/getting-started/quick-start.md).

## Common Workflows

**Deploy with zero downtime**
```bash
phelix rebuild myapp --blue-green      # or: --replicas 3 for rolling
```
→ [Zero-downtime deployments](docs/guides/zero-downtime-deployments.md)

**Ship to a slice of traffic first**
```bash
phelix rebuild myapp --canary 5        # 5% canary, verify, then promote
```
→ [Canary & progressive rollouts](docs/guides/canary-and-progressive-rollouts.md)

**Roll back**
```bash
phelix rollback myapp --to v7 --dry-run   # preview, then run without --dry-run
```
→ [Rollback](docs/guides/rollback.md)

**Build across toolchains and platforms**
```bash
phelix build myapp --matrix --go-versions 1.26,1.27 --platforms linux/amd64,linux/arm64
```
→ [Matrix builds](docs/guides/matrix-builds.md)

**Deploy on push**
```bash
PHELIX_WEBHOOK_SECRET=… phelix webhook
```
→ [Git webhook deploys](docs/guides/webhooks.md)

## Examples

Runnable, dependency-free starter projects under [`examples/`](examples/):

- [Simple Go application](examples/simple-go/) — the minimal `PORT`-reading app.
- [Simple Rust application](examples/simple-rust/) — the same, in Rust (stdlib only).
- [Blue-green deployment](examples/blue-green/) — a zero-downtime deploy you can watch cut over.
- [Docker runtime (Go)](examples/docker-go/) — Phelix runs the app as a container.
- [Docker runtime (Rust)](examples/docker-rust/) — the same, in Rust.

## Documentation

### Getting Started
- [Installation](docs/getting-started/installation.md)
- [Quick Start](docs/getting-started/quick-start.md)
- [The PORT Contract](docs/getting-started/the-port-contract.md)

### Guides
- [Authentication](docs/guides/authentication.md)
- [Interactive Usage](docs/guides/interactive-usage.md)
- [Zero-Downtime Deployments](docs/guides/zero-downtime-deployments.md)
- [Canary & Progressive Rollouts](docs/guides/canary-and-progressive-rollouts.md)
- [Rollback](docs/guides/rollback.md)
- [Health Checks](docs/guides/health-checks.md)
- [Environment Variables](docs/guides/environment-variables.md)
- [Build Reports](docs/guides/build-reports.md)
- [Matrix Builds](docs/guides/matrix-builds.md)
- [Docker Image Building](docs/guides/docker-images.md)
- [Docker Runtime](docs/guides/docker-runtime.md)
- [Monitoring](docs/guides/monitoring.md)
- [Webhooks](docs/guides/webhooks.md)
- [Resource Limits](docs/guides/resource-limits.md)
- [Security](docs/guides/security.md)
- [Troubleshooting](docs/guides/troubleshooting.md)
- [Best Practices](docs/guides/best-practices.md)

### Reference
- [CLI Commands](docs/reference/commands.md)
- [Configuration (`phelix.yaml`)](docs/reference/configuration.md)
- [Environment Variables](docs/reference/environment.md)
- [Data Directory](docs/reference/data-directory.md)
- [Error Codes](docs/reference/error-codes.md)
- [Exit Codes](docs/reference/exit-codes.md)

### Architecture
- [Overview](docs/architecture/overview.md)
- [Deployment Engine](docs/architecture/deployment.md)
- [State Management](docs/architecture/state-management.md)
- [Build System](docs/architecture/build-system.md)
- [Monitoring](docs/architecture/monitoring.md)
- [Identity](docs/architecture/identity.md)
- [Error Architecture](docs/error-architecture.md)
- [Backend Contracts](docs/architecture/backend-contracts/)

## Architecture

Phelix is a single Cobra binary: thin commands in `cmd/` orchestrate domain
packages in `internal/`, and all runtime state lives on disk under `~/.phelix/`.

```text
your app  →  phelix CLI  →  build / deploy engine  →  runtime (host process or container)
                                     │                        │
                                     ▼                        ▼
                          versioned state on disk      phelix proxy (public port)
                                                              │
                                                              ▼
                                            optional gRPC monitoring → dashboard
```

Deploys are fail-safe by construction: a version is only promoted to `current`
after its health check passes, the proxy switches targets atomically, and a failed
candidate never touches the live instance. See the
[architecture overview](docs/architecture/overview.md).

## Security

Environment variables and registry credentials are encrypted at rest with
AES-256-GCM and never logged in plaintext; authentication is optional and
tokens are stored locally; the monitor gRPC channel uses TLS (only `mode: dev`
uses plaintext, for local backends); webhook secrets live only in environment
variables and signatures are verified constant-time; managed containers publish
only to `127.0.0.1`. When you are not logged in, no app data, metrics, or events
leave your machine. See [Security](docs/guides/security.md).

## Support

- Documentation: [phelix.anophel.com/docs](https://phelix.anophel.com/docs)

## License

No license file is currently present in this repository; the licensing status is
unresolved. See the repository's license metadata for the authoritative status
before relying on it.
