# Build Reports & Regression Alerts

Every successful native Go/Rust build (`build`, `rebuild`, zero-downtime
deploys) automatically prints a **Build Report** and records it with the version
metadata — no flags, no configuration, no network:

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

The report captures: application, version, language, compiler/toolchain version
(`go version` / `rustc --version`), build start/end time, duration, binary size
and target platform, cache status (`COLD` = real compilation happened, `HIT` =
served from the compiler's build cache), git commit when available
(`unavailable` outside a git repository), and the build arguments you passed via
`--build-arg`.

## The critical cache rule

> Build duration comparisons are only made between comparable cache modes. A
> cold build is never directly compared against a cache-hit build for duration
> regression — the report explicitly says `Comparison skipped` instead of
> inventing a misleading percentage.

> Binary size remains a valid regression signal across cache states when the
> artifact, toolchain, platform, and matrix context are comparable.

## Regression analysis

Regression analysis compares the current build against the previous **5
comparable builds** (same application, language, toolchain, target platform,
artifact type — cache state ignored for size, matched for duration). Alerts fire
on:

- **Binary size**: growth ≥ 1 MB **or** ≥ 5% versus the previous comparable
  build, plus a `N-build average` baseline when enough history exists.
- **Build duration**: slowdown ≥ 25% **and** ≥ 2s versus a previous build in the
  *same* cache mode (thresholds avoid noisy micro-changes).

Regression analysis is pure observability: missing history never fails a build,
and a reporting problem only prints a warning.

## Inspecting stored reports — `phelix build-report`

Read-only inspection of stored build reports — never triggers a build, works
fully offline:

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

Versions recorded before build reporting existed show `no build report
metadata`.

## Where the metadata lives

Each version row in `versions.json` carries an additive `build_report` object
with the metrics captured during that build. The field is optional and additive:
versions recorded by older Phelix releases (and Docker-image versions) simply
lack it. Missing report metadata only makes that version unavailable for
regression comparisons — rollback and everything else keep working. See
[state management](../architecture/state-management.md) for the schema.

For matrix builds, regression analysis is combination-aware — see
[Matrix builds](matrix-builds.md).

## Related

- [Command reference](../reference/commands.md#building--rebuilding).
- [Matrix builds](matrix-builds.md) — per-combination build reports.
