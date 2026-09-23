# Quick Start

This guide takes you from an installed Phelix to a running, versioned,
zero-downtime-capable application.

> Prerequisite: Phelix installed (see [Installation](installation.md)) and a Go
> or Rust project in the current directory whose app listens on the port given
> by the `PORT` environment variable (see [The PORT Contract](the-port-contract.md)).
> If you don't have one yet, copy the runnable
> [`examples/simple-go`](../../examples/simple-go/) starter and follow along.

## The essential commands

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

## The lifecycle

A typical application lifecycle:

```text
build    →  (rebuild --blue-green)  →  rollback    →  stop / remove
   \            |                          ^
    \           +-- versioned (v1, v2…) ----+
```

1. **Build** a project into a named, managed app and start it.
2. **Rebuild** after changes; optionally zero-downtime via blue-green or rolling.
3. Every build is **versioned** (`v1`, `v2`, …) and can be tagged.
4. Every successful build automatically records a **Build Report** (compiler,
   toolchain version, duration, cache status, binary size, commit) and compares
   it against previous comparable builds, surfacing binary-size and build-time
   regressions.
5. **Roll back** to any retained version if something breaks.
6. Monitor with `status`, `list`, `log`, and `health`.
7. Ship containers with `dockerize`.

## Understanding the result

- Every successful build creates a **numbered version** (`v1`, `v2`, …) rather
  than overwriting — see [Rollback](../guides/rollback.md).
- `phelix build` **creates** the app; `phelix rebuild` only works on an app that
  already exists.
- Lifecycle commands (`start`, `stop`, `restart`, `status`, `list`, `log`,
  `remove`) operate on the managed app — see the
  [command reference](../reference/commands.md).
- Zero-downtime deploys require the `phelix proxy` daemon — see
  [Zero-downtime deployments](../guides/zero-downtime-deployments.md).

## Where to go next

- [Configuration](../reference/configuration.md) — put the app name, port,
  health checks and deploy strategy in `phelix.yaml` so you never repeat them.
- [Authentication](../guides/authentication.md) — optional login to sync to the
  dashboard.
- [Command reference](../reference/commands.md) — every command, flag, and
  default.
- [Troubleshooting](../guides/troubleshooting.md) — when a build, deploy, or
  health check does not go as expected.
- [Examples](../../examples/) — runnable Go, Rust, and blue-green starters.
