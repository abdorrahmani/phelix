# Build System

Covers the builders, toolchain handling, and the matrix engine. For the
user-facing matrix workflow see [Matrix builds](../guides/matrix-builds.md); for
build reports see [Build reports](../guides/build-reports.md).

## Builders & toolchain

`internal/builder` — `BuildManager` detects language (`Cargo.toml` → Rust;
`main.go`/`go.mod` or any `package main` `.go` file → Go) via `BuilderFactory`.

`internal/toolchain` — `EnsureTool` checks PATH, prompts to install (via
`cmd.Confirm`), and falls back to OS-specific manual instructions in
`toolchain/manual.go`. A missing toolchain surfaces as `TOOLCHAIN_NOT_FOUND` with
the error-reporter's install guidance (see [error codes](../reference/error-codes.md)).

## Build report lifecycle

`internal/buildreport` captures metrics for every successful native build
(compiler/toolchain version, start/end time, duration, cache status/source, binary
size + platform, git commit, build args) and runs regression analysis against the
previous 5 comparable builds. Reports are stored inside `versions.json` per version
(`build_report`), not in a separate database, and everything works offline. Cache
modes are matched for duration comparisons and ignored for size. See
[Build reports](../guides/build-reports.md) for the thresholds and the cache rule.

## Matrix engine

`internal/matrix` — plan, executor (bounded worker pool), reporters, and Go/Rust/
Docker builders.

### Dimensions and expansion

The matrix is a set of **dimensions** (`lang`, `version`, `os`, `arch`,
`variant`). The Cartesian product of configured versions × platforms is only the
*base*; the full pipeline is deterministic and identical for CLI flags,
`phelix.yaml`, the wizard, and any future remote execution:

```text
Base Cartesian product  →  Include rules  →  Exclude rules  →  Final combinations
```

- **Include** rules add a full combination not in the base product, or merge
  metadata (e.g. `tag: latest`) into matching combinations; a well-formed rule that
  matches nothing is a configuration error (a typo can't silently shrink the
  matrix).
- **Exclude** rules are partial matchers applied *after* includes; unknown keys are
  rejected outright.
- Per-dimension precedence: `CLI explicit value > phelix.yaml matrix profile >
  command default`. List dimensions are replaced, never merged. Validation is
  identical wherever config comes from; an invalid profile fails every command that
  reads `phelix.yaml`, naming the YAML location.

### Runs, run locks, and orphan handling

Every execution gets a **Matrix Run ID** (`mx_YYYYMMDD_hhhh`), persisted under
`~/.phelix/matrix/runs/` (`<id>.json` record, `<id>.lock` execution lock,
`<id>.manifest.json` release manifest). The run snapshots the effective
configuration at execution time, so later `phelix.yaml` edits never rewrite what an
old run built.

- **Incremental persistence.** Runs are written after every attempt start and every
  completed combination, so there is no second status system that can drift; live
  `matrix status` reads the same records.
- **Run locks.** A per-run lock guards against two concurrent executions of the same
  run; a lock left by a dead process is reclaimed automatically after a restart.
- **Orphan handling.** A persisted `running` run whose executing process is gone
  (crash, SIGKILL, restart) is reported as **orphaned** — resumable, its in-flight
  combinations shown as `stale`. A completed run never reports `running`.
- **Recovery mechanisms are distinct:** automatic retries (`--matrix-retries`,
  same run, transient failures only), resume (`--resume`, same run, incomplete
  combinations only), and manual retry (`matrix retry --failed`, a new linked run
  recording `parent_run_id`).

### Artifact integrity and the release manifest

Every successful artifact carries a streamed **SHA-256 checksum** (for Docker
artifacts, the image content digest). A checksum that cannot be computed is an
artifact-integrity failure — the combination is recorded as failed and never
counted as a valid release artifact; integrity failures are never auto-retried.

A matrix build is **one logical release with many artifacts**: `versions.json`
records exactly one version row (`vN`) carrying all combination artifacts. Each
finished run with successful artifacts writes a release manifest
(`<run-id>.manifest.json`) whose `status` is fail-closed:

| Run outcome | Manifest |
|-------------|----------|
| all succeeded | written, `status: "complete"` |
| some succeeded, some failed | written, `status: "partial"` (only successful artifacts) |
| nothing succeeded / interrupted / still running | **not written** |

Docker matrix builds record image digests per artifact in `versions.json` but
produce no run manifest (they are not Matrix Runs).

### Cross-compilation

- **Go**: native `GOOS`/`GOARCH` (+ `GOARM`) with `CGO_ENABLED=0`; falls back to a
  per-version Docker container when no matching host toolchain exists, so an
  artifact is never labeled with a toolchain it wasn't built with.
- **Rust**: the [`cross`](https://github.com/cross-rs/cross) tool inside Docker,
  pinning the toolchain via `cross +<version>`.

Build commands are passed as separate arguments (never through a shell), with
target env injected via `docker run -e`. Failure handling is fail-open for the
build phase (report all, exit non-zero on any failure) and fail-closed for the
Docker push phase (`--push-partial` to override).

## Related

- [Matrix builds](../guides/matrix-builds.md) — user workflow and full flag set.
- [State management](state-management.md) — how versions and artifacts are recorded.
- [Build reports](../guides/build-reports.md) — the per-build report and
  regression rules.
