# Workspaces

A workspace treats a monorepo as one unit: one manifest describes the mesh, and
one command builds, deploys, rolls back, or reports on every service in the
right order. Without it you configure and run Phelix once per service, with no
shared deployment order and no unified view.

Phelix discovers workspaces two ways:

| Source | How it is found |
|---|---|
| `phelix.workspace.yaml` | Explicit manifest in the workspace root. It wins when present. |
| `go.work` | Native Go workspace. The `use` entries are the member modules. |
| `Cargo.toml` | Native Cargo workspace. `members` (or `default-members`) minus `exclude`, globs expanded. |

Discovery only considers members Phelix can run: a Go module with a `main`
package, or a Rust crate with a `[[bin]]` target or `src/main.rs`. Library-only
members belong to the language workspace but are not deployable services, so
they are skipped. A `go.work` file listing a missing module fails the whole
workspace instead of silently shrinking it.

Workspaces are read from the current directory only. There is no ancestor
search, matching how `phelix.yaml` is resolved.

## The manifest

```yaml
# phelix.workspace.yaml: define once, for the entire mesh.
name: shop                       # optional; defaults to the root directory name
discover: false                  # merge native workspace members not named here

services:
  auth:
    path: ./services/auth
    port: 8081
    strategy: blue-green
  orders:
    path: ./services/orders
    port: 8082
    depends_on: [auth]
  gateway:
    path: ./gateway
    port: 8080
    strategy: rolling
    replicas: 3
    depends_on: [auth, orders]
```

| Field | Meaning |
|---|---|
| `path` | Service directory, relative to the workspace root. Must exist; paths escaping the root are rejected. |
| `port` | Public port. Leave it out to have one assigned from `--port-base`. |
| `strategy` | `classic`, `blue-green`, or `rolling`. Empty defers to the service's own `phelix.yaml`, then its persisted deploy mode. |
| `replicas` | Rolling replica count. Only valid together with `strategy: rolling`. |
| `depends_on` | Services that must be built or deployed first. Cycles and unknown names are rejected. |

Service names follow app-name rules, because every service is registered as a
Phelix app: the name keys its `deploy.json`, `versions.json`, and health config.
Ports must be unique across the workspace when declared explicitly.

Canary and progressive rollouts are deliberately not available in a manifest.
They verify traffic metrics over time for one service, whereas a workspace
deploy halts the whole mesh on the first failure. A service whose own
`phelix.yaml` asks for either degrades to classic inside a workspace deploy.

`discover: true` merges runnable members of a native `go.work` or Cargo
workspace into the manifest. Explicit entries win on a name collision.

## Commands

```bash
phelix build --all        # topological build; parallel within each level
phelix deploy --all       # depends_on order; the first failure halts the mesh
phelix status --workspace # version, health, port, strategy for every service
phelix rollback --all     # previous version for every service, reverse order
```

### `phelix build --all`

Services are grouped into dependency levels. Every service in a level has all of
its dependencies satisfied, so the level builds in parallel:

```
auth            level 0
orders          level 1  (depends on auth)
gateway         level 2  (depends on auth, orders)
```

`--concurrency` (default 3) bounds how many services of one level compile at a
time. A failing service stops the run: no service of a later level starts, and
every service that did run records its outcome in the workspace state.

Builds use the two-phase version rule. A compiled service gets a version with
`is_current: false`; only a successful deploy promotes it. A service that
compiles but will not start stays inspectable on disk and never becomes current.

### `phelix deploy --all`

Deploys in strict `depends_on` order, one service at a time, so an upstream
consumer never comes up against a dependency that has not deployed yet. Each
service keeps its own port, health checks, and strategy, and runs through the
same rebuild path a single app uses, including per-app deploy locks and health
gating.

The first failure halts the run. `--auto-rollback` restores the failing
service's previous version before halting, so that service keeps serving while
you investigate.

### `phelix status --workspace`

One table for the mesh. Status is derived from deployment reality: a service is
`running` only when its serving process is alive, never because a state file
says so. A blue-green or rolling service reports the lifecycle of its active
slot. A classic service, which keeps no `deploy.json`, reports the reconciled
app record instead.

Health is a single one-shot probe of each configured endpoint, bounded at two
seconds per probe. A service with no health configuration shows `-`, which
means "not configured", never "unhealthy".

### `phelix rollback --all`

Reverts every service to its own previous version in reverse dependency order,
so consumers are taken down before the providers they rely on. A failure halts
the rest of the run. `--all` cannot be combined with `--to`, `--list`,
`--dry-run`, `--verify`, or `--request-key`, which all describe one service.

## State and locking

Workspace state lives under the Phelix data directory:

```
~/.phelix/workspaces/<name>/workspace.json   membership + last outcome per service
~/.phelix/workspaces/<name>/workspace.lock   orchestration lock
```

Each service's runtime truth stays in its own stores (`apps.json`,
`deploy.json`, `versions.json`). The workspace file records membership and the
last orchestration outcome so scripts can inspect what the last run did; it is
not a second source of runtime truth.

A workspace-level OS file lock serializes whole-mesh operations against each
other. The kernel releases it when the holder dies, so a crashed build or deploy
never leaves a stale lock. Each service still takes its own deploy lock inside,
so a workspace operation never bypasses single-app safety. A contended
workspace lock fails fast with `WORKSPACE_LOCKED` (exit 12).

## Machine output

`build --all`, `deploy --all`, `status --workspace`, and `rollback --all`
emit one JSON envelope for the whole run, not one per service. Per-service
progress moves to stderr while the envelope owns stdout.

```bash
phelix status --workspace --json | jq '.result.services[] | {name, status, version, port}'
```

## Limitations

- Native `go.work` and Cargo members get no `depends_on` unless a manifest
  declares one. Native workspace files carry no service dependency information,
  so every member builds in one level.
- `build --all` supports native Go and Rust services only. A service with
  `deploy.runtime: docker` is rejected and must be built and deployed on its
  own.
- `deploy --all` requires services to be registered first. Run
  `phelix build --all` once so each service has a directory and port on record.
