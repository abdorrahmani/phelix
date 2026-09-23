# Matrix Builds

Build the cross-product of **toolchain version × platform** in one command. The
matrix can be configured three ways — CLI flags, a `matrix:` profile in
`phelix.yaml`, or the interactive wizard — all three converge into the same
configuration and are validated identically.

> In the README this section was nested under "Run Phelix in Docker"; matrix
> builds are a standalone feature and are documented here on their own. Deep
> implementation details live in
> [build-system architecture](../architecture/build-system.md).

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

## Matrix profile in `phelix.yaml`

The same matrix can be configured persistently:

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
The profile goes through the exact same validation and expansion as CLI flags.

## Dimensions, include, and exclude

Internally the matrix is a set of **dimensions** (`lang`, `version`, `os`,
`arch`, `variant`); the Cartesian product of the configured versions × platforms
is only the *base* of the expansion. The full pipeline is deterministic and
identical for CLI flags, `phelix.yaml`, the wizard, and any future remote
execution:

```text
Base Cartesian product  →  Include rules  →  Exclude rules  →  Final combinations
```

**Exclude** rules are partial matchers: a rule matches every combination that
carries the constrained values, regardless of the other dimensions. Rule keys:
`go`/`rust` (shorthand for ecosystem + version), `lang`, `version`, `platform`,
`os`, `arch`, `variant`. So this drops *all* Windows combinations of Go 1.25 and
nothing else:

```yaml
exclude:
  - go: "1.25"
    platform: windows/amd64
```

**Include** rules do two things:
- a rule that names a full combination (ecosystem + version + platform) **adds
  it** when the base product doesn't contain it — `go: "1.28"` above builds 1.28
  even though only 1.25–1.27 are configured;
- any other keys on an include entry (like `tag: latest`) are **metadata** merged
  into the matching combination(s) — recorded in the run and visible in `matrix
  show`.

A duplicate include (a combination already in the matrix) never schedules the job
twice — it only merges its metadata. A well-formed rule that matches no
combination at the point it is applied is a **configuration error**, so a typo
cannot silently shrink (or fail to shrink) the matrix. Exclude rules reject
unknown keys outright for the same reason. Excludes apply *after* includes, so an
exclude can remove an included combination.

## Configuration precedence

Per dimension, deterministic:

```text
CLI explicit value  >  phelix.yaml matrix profile  >  command default
```

- List dimensions are **replaced, never merged**: `phelix build --go-versions
  1.28` next to the profile above builds only `1.28` (the YAML version list is
  overridden entirely), while unmentioned dimensions keep their configured values
  (platforms and concurrency above stay from the profile).
- `--matrix=false` explicitly disables an enabled profile; `--matrix` alone uses
  the configured profile when one exists.
- Versions and platforms are validated identically wherever they come from: an
  invalid profile fails `phelix build` (and every command that reads
  `phelix.yaml`) with an error naming the YAML location, e.g. `configuration
  error: matrix.go.versions: ...`.

## Matrix runs: list, show, status, retry, wizard

Every matrix execution gets a **Matrix Run ID** (`mx_20260909_8f31`) — printed
during the build, recorded in `builds/matrix/report.json` (`run_id`), attached to
each artifact in `versions.json` (`matrix_run_id`), and persisted locally under
`~/.phelix/matrix/runs/`. The run snapshots the effective configuration at
execution time, so later `phelix.yaml` edits never rewrite what an old run says it
built.

```bash
phelix matrix list                 # recorded runs, newest first (--limit N caps; default 20, 0 = all)
phelix matrix list --json          # machine-readable summaries
phelix matrix show mx_20260909_8f31        # config snapshot + per-combination results
phelix matrix show mx_20260909_8f31 --json
phelix matrix status               # live state of the active run (or "No active Matrix Run.")
phelix matrix status mx_20260909_8f31      # a specific run's state
phelix matrix status --json        # machine-readable current state
phelix matrix retry mx_20260909_8f31 --failed   # retry a run's failures in a new linked run
phelix matrix init                 # interactive wizard → writes phelix.yaml
```

`matrix show` displays the configuration snapshot (with each dimension's origin:
`cli`, `phelix.yaml`, or `default`, plus the effective include/exclude rules and
retry budget), every combination's status, duration, attempt history, artifact,
SHA-256, and redacted error details for failures. An unknown run ID is a clean
`NOT_FOUND` error. Local run history can be relocated with `PHELIX_DATA_DIR` like
all Phelix state.

Run statuses: `succeeded` (everything passed), `partial` (some passed, some
failed), `failed` (nothing passed), `interrupted` (stopped with incomplete
combinations — resumable). A run with failures never reports `succeeded`.

### Live status

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

`matrix status` answers *"what is happening right now?"* — unlike `matrix list`
(all recorded runs) and `matrix show` (full inspection). With no argument it
selects the **active run**: a run whose persisted status is `running` *and* whose
execution lock is held by a live process. When several runs execute concurrently
the most recently started one is shown (with a note); name a run explicitly to
inspect another. If nothing is executing it prints `No active Matrix Run.` and
points at the most recent run — never a fabricated status. A persisted `running`
run whose executing process is gone (crash, SIGKILL, restart) is reported as
**orphaned** — resumable, its in-flight combinations shown as `stale`. Counters
are always internally consistent (`success + failed + running + pending + skipped
= total`).

## Automatic retries

```bash
phelix build myapp --matrix --matrix-retries 2
```

`--matrix-retries 2` means **two additional attempts** (at most 3 executions per
combination), not two total. Only failed combinations are retried — a combination
that succeeded is never re-executed. Retries happen inside the *same* Matrix Run:
the run records every attempt (`Attempts: 3, final: succeeded`) in `matrix show`
and `report.json`. Only *transient* failures are retried (network, connection,
timeout, Docker daemon unavailable); deterministic failures — compiler errors,
invalid versions, configuration problems — fail on the first attempt and are not
repeated.

## Resume

```bash
phelix build myapp --matrix --resume            # most recent interrupted run
phelix build myapp --matrix --resume=mx_20260909_8f31
```

Resume continues an **existing** incomplete run instead of starting a new matrix.
Runs persist incrementally (after every completed combination), so resume works
after Ctrl-C, a crash, or a machine restart. Exact semantics:

- `succeeded` combinations → **never rebuilt**
- `failed` combinations → skipped (retry them explicitly with `phelix matrix retry
  <run-id> --failed`)
- `pending` combinations and `running` combinations whose worker is gone →
  **executed**

Resume uses the run's original configuration snapshot (dimensions, include/exclude
rules, build args) — later `phelix.yaml` edits cannot change what the resumed run
builds. Explicit `--build-arg` flags on the resume invocation are the one
exception: they replace the snapshot's build args. A per-run lock file guards
against two concurrent executions of the same run; a lock left by a dead process
is reclaimed automatically after a restart. The first Ctrl-C stops the run cleanly
(in-flight builds are killed and stay pending); a second one terminates
immediately. `--matrix-dry-run` alongside `--resume` previews the combinations a
resume would execute without touching the run.

## Manual retry

```bash
phelix matrix retry mx_20260909_8f31 --failed
```

A manual retry **creates a new run** containing only the combinations whose final
status in the source run is `failed`. The new run records its parent
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
include entries and exclusions (picked from the real expanded list), then previews
the *actual* expanded combination list with base/included/excluded counts — never
a naive versions × platforms count — before writing the profile. It preserves all
unrelated `phelix.yaml` keys and comments, offers Edit / Keep / Disable / Cancel
when a profile already exists, and refuses to run without an interactive terminal
(so it never hangs in CI).

## Artifact checksums and the release manifest

Every successful matrix artifact carries a **SHA-256 checksum** computed from the
final artifact bytes — streamed, never loaded into memory wholesale. For Docker
image artifacts (matrix dockerize), the checksum is the image's content digest
(`docker image inspect`), Docker's own SHA-256 of the image.

Checksums appear in the build summary and `matrix show` / `matrix status`
(shortened, e.g. `SHA256: 3263d01d02d8…`), and in full, machine-readably, in
`builds/matrix/report.json` (`sha256` per combination), `matrix status --json`,
each artifact row in `versions.json` (`sha256`), and the release manifest below.

A checksum that cannot be computed is an **artifact-integrity failure**: the
combination is recorded as failed — an artifact whose bytes cannot be verified is
never recorded as a valid release artifact (and integrity failures are never
auto-retried).

> SHA-256 here is an **integrity** mechanism — it answers "did the artifact bytes
> change?". It does **not** provide authenticity ("who produced this artifact?");
> Phelix has no artifact signing.

**One logical release, many artifacts.** A matrix build is one logical
build/release of one application version that produces multiple artifacts. Matrix
combinations are **artifacts of that release, never independent application
versions**: `versions.json` records exactly **one** version row (`vN`) carrying
all of them:

```text
Application
    └── Version vN (one versions.json row)
            └── Matrix Run mx_…
                    └── Artifacts: go1.27-linux-amd64, go1.27-linux-arm64, …
```

Each finished run with successful artifacts also writes a **release manifest**
describing that artifact set, stored next to the run record as
`<PHELIX_DATA_DIR>/matrix/runs/<run-id>.manifest.json`. Completeness is explicit
and fail-closed:

| Run outcome | Manifest |
|-------------|----------|
| all combinations succeeded | written, `status: "complete"` |
| some succeeded, some failed | written, `status: "partial"` (only successful artifacts listed) |
| nothing succeeded / interrupted / still running | **not written** |

A partial matrix can therefore never masquerade as a complete release — the
`status` field and the `total_combinations` count make the gap machine-checkable.
Docker matrix builds (`phelix dockerize --matrix`) record image digests per
artifact in `versions.json` but produce no run manifest (they are not Matrix
Runs).

## How matrix builds work

**Cross-compilation strategy:**
- **Go**: native cross-compilation via `GOOS`/`GOARCH` (plus `GOARM=6`/`7` for the
  ARM variant platforms) with `CGO_ENABLED=0` — no extra toolchain needed. CGO
  projects fail with a clear error suggesting Docker-based builds or a C
  cross-compiler. If no host Go toolchain is installed — or the host toolchain is
  a different Go version than the requested one — the build automatically falls
  back to the Docker path, so an artifact is never labeled with a toolchain it was
  not built with.
- **Rust**: uses the [`cross`](https://github.com/cross-rs/cross) tool (not raw
  `rustup target add`), building inside a Docker container with the correct
  linker/C libraries pre-configured. The requested version is pinned via `cross
  +<version>`.

**Matrix builds don't need a port:** they compile artifacts for other platforms
and never start a local instance, so port validation and availability checks are
skipped.

**Output naming** (every combination produces a unique, path-safe artifact):

| Artifact | Naming |
|----------|--------|
| Binary | `builds/matrix/{lang}{version}-{os}-{arch}[-{variant}]/{app}_{arch}_{lang}_{version}[_{variant}]` |
| Docker per-combination tag | `{app}:{tag\|latest}-{lang}{version}-{arch}[-{variant}]` |
| Docker multi-arch manifest | `{app}:{tag\|latest}` for a single combination, version-qualified (`{app}:{tag}-{version}`) when the run produced more than one combination |
| JSON report | `builds/matrix/report.json` |
| Release manifest | `<PHELIX_DATA_DIR>/matrix/runs/<run-id>.manifest.json` |

**Docker matrix builds are linux-only:** Docker images cannot target `darwin/*` or
`windows/*`, so those combinations fail fast with a pointer to `phelix build
--matrix`. Per-combination `TARGET*` build args (`TARGETPLATFORM`, `TARGETOS`,
`TARGETARCH`, `TARGETVARIANT`, `TARGETVERSION`) are passed to every image build,
and the requested toolchain version is additionally passed as
`GO_VERSION`/`RUST_VERSION`. A Dockerfile is generated when the project has none.

> **Note:** `--matrix-tags` is kept for compatibility but is a no-op —
> per-combination tagging is the default behavior of every matrix dockerize.

**Failure handling:**
- **Build phase**: fail-open — one failure doesn't stop the rest; full summary at
  the end. The CLI exits non-zero when any combination failed.
- **Push phase**: fail-closed by default — if any combination failed, nothing is
  pushed. Use `--push-partial` to push only successful images.

**Independent build reports per combination:** every combination retains its own
build metrics — toolchain version, target platform, duration, cache status and
binary size — recorded alongside the matrix version's artifacts in `versions.json`
and mirrored in `builds/matrix/report.json`. Regression analysis is
combination-aware: `Go 1.27 / linux-amd64` is only ever compared against previous
`Go 1.27 / linux-amd64` builds, never against `Go 1.26 / linux-arm64` or a Rust
build.

## Related

- [Build reports](build-reports.md) — the per-build report and regression rules.
- [Docker image building](docker-images.md) — the single-image dockerize the
  matrix reuses.
- [Build-system architecture](../architecture/build-system.md) — dimension model,
  expansion pipeline, run locks, orphan handling.
- [Command reference](../reference/commands.md#matrix-builds).
