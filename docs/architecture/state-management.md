# State Management

All Phelix runtime state lives on disk under `~/.phelix/` (relocatable with
`PHELIX_DATA_DIR`). The [data-directory reference](../reference/data-directory.md)
lists every file; this document explains the *model* and the invariants.

## Two state models, deliberately separate

| File | Owner | Model |
|---|---|---|
| `~/.phelix/apps.json` | `internal/app` (`AppManager`) | single-PID `AppInfo` used by `start`/`stop`/`status`/`list` |
| `~/.phelix/apps/<App>/deploy.json` | `internal/deploy` | rich blue-green slots / rolling replicas |
| `~/.phelix/apps/<App>/versions.json` | `internal/deploy/version.go` | version history + build reports + matrix artifacts |

`deploy.json` exists so zero-downtime deploys never have to distort the single-PID
model. Lifecycle state shown by `list`/`status` is **derived from deployment
reality** (`internal/deploy/lifecycle.go`: `InstanceAlive`, `ServingInstance`), not
read from a possibly-stale `apps.json` record.

## Versioning is the source of truth for build history

`internal/deploy/version.go`:

- `versions.json` holds a monotonic list of `VersionMeta` (version, tag, git
  commit, size, deploy mode, `is_current`, optional Docker image / matrix artifacts
  / multi-arch image, and an optional additive `build_report`).
- `RecordFreshBuild` / `RecordDockerBuild` / `RecordMatrixBuild` append entries
  (always `is_current=false`) and run pruning.
- `PromoteVersion` flips `is_current`, sets `deployed_at`, and updates the
  `current → builds/vN` symlink.
- `PruneVersions` enforces retention while never deleting the active version.
- `ResolveVersionOrTag` accepts `v3`, `3`, or a tag (errors on ambiguous tags).

## Two-phase version promotion

```text
build/rebuild ──► RecordFreshBuild (is_current=false, copy binary → builds/vN)
              │        + prune to retention (never drop active)
              ▼
        deploy succeeds (start / health check)
              │
              ▼
        PromoteVersion (is_current=true, deployed_at, current → builds/vN symlink)
```

- **A build records `is_current=false`.** Only a successful deploy/health check
  calls `PromoteVersion`, which flips the flag, sets `deployed_at`, and repoints
  the `current → builds/vN` symlink.
- **A binary that compiles but won't start stays inspectable on disk and never
  becomes current.** A broken deploy can never corrupt the "current" pointer.
- Versions store both the binary (`builds/vN/binary`) and its paired encrypted env
  snapshot (`env/vN.enc`), so rollback always restores a known-good binary + env
  pair — never binary-only.

The `build_report` field is optional and additive: versions recorded by older
releases (and Docker-image versions) simply lack it. A missing or malformed
`build_report` only makes that version unavailable for regression comparisons —
rollback, listing, status, version loading, retention/pruning and the `current`
symlink logic all keep working.

## Rollback state

`rollback` inspects `deploy.json`:

- **No deploy state** → classic path: stop, copy `builds/vN/binary` to the app
  location, start, then promote.
- **Blue-green/rolling state present** → zero-downtime path via the proxy: start
  the target version on an internal port, health-check it, atomically switch the
  proxy, then stop the old instance.

In both paths the version is only promoted *after* the start/switch succeeds.
Rollback audit history is a per-app JSON-Lines file
(`rollback_history.jsonl`) plus `rollback.log`; see
[Rollback](../guides/rollback.md).

## Locking

Deploy mutual exclusion is an **OS file lock** (`flock`/`LockFileEx` on
`deploy.lock`), not a check-then-write on `deploy.json`. The kernel releases it on
process death, so stale locks are impossible by construction. Rollback shares this
lock so it cannot race a concurrent deploy or rollback on the same app.

## Retention

Old versions are pruned after each successful build, keeping the last **5** by
default (configurable per plan tier: Free 3, Pro 10, Enterprise unlimited). The
currently active version (`is_current`) is never pruned, even if it falls outside
the retention window.

## Identity in state

`apps.json` persists the per-app `app_id`, independent of agent, server, and app
name. See [identity](identity.md).

## Related

- [Deployment engine](deployment.md) — how these files are read/written during a
  deploy.
- [Rollback](../guides/rollback.md), [Build reports](../guides/build-reports.md).
- [Data directory reference](../reference/data-directory.md) — the full on-disk
  layout.
