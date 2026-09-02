# Phelix Error Architecture

Phelix has a single structured error pipeline: domain and infrastructure code
returns and wraps errors, the CLI renders them exactly once on stderr, and a
thin known-error reporter adds explanations and fix suggestions on top.

This document describes that pipeline, the error reporter, and the rules for
extending both. The exit-code table and user-facing behavior are documented in
the [README](../README.md#error-handling).

## Core pieces

| Piece | Location | Responsibility |
|---|---|---|
| Structured error | `internal/errors` (package `phelixerr`) | `Code`, `Error{Code, Message, Err}`, `Wrap/Wrapf/New/Newf`, `CodeOf`, `IsCode`, `Cause`, `Redact` |
| Known-error reporter | `internal/errreport` | ordered resolver registry; explanation + fix + command + docs for recognized failures |
| CLI render boundary | `cmd/cli_errors.go` | `RenderError` / `renderCLIError` / `ExitCodeFor` / `hintFor` — the only place errors are rendered |
| Tool-output capture | `internal/builder` (`ToolError`) | bounded tail of failed go/cargo output, redacted at render |
| Port/process lookup | `internal/port` (`FindListener`) | read-only, best-effort listener identification |

## Error flow

```
  infra/domain:  exec.Run · net.Listen · os.Open  → root error (exec.ExitError, ENOENT, url.Error)
       │   phelixerr.Wrapf(code, err, "context")   → outermost = code context; Cause = root (errors.Is/As)
       ▼
  cmd boundary:  main.go → RenderError(err, --debug)
       │   stderr, once, redacted, exit code per category
       ├── errreport.For(err)      → known → report (explanation / fix / command / docs)
       │                           → unknown → generic hint (unchanged behavior)
       ├── errreport.ToolOutput()  → captured tool output, redacted, bounded (both paths)
       └── debug                   → Cause: redacted root cause chain
       ▼
  exit code per the documented table (BUILD=20, NETWORK=30, CONFIG=40, …)
```

Layer responsibilities:

- **Infrastructure** (`internal/proxy`, `internal/docker`, `internal/toolchain`,
  `internal/grpc`, `internal/server`): wrap with technical context, preserve
  the root cause, never embed command/compiler/docker output in messages.
- **Domain** (`internal/app`, `internal/deploy`, `internal/env`, `internal/health`,
  `internal/matrix`, `internal/builder`): attach the structured `Code`
  (category, not message); messages are identity-rich and internals-free.
- **CLI** (`cmd/`): the only render surface — stderr, once, redacted, with
  hints/report/exit-code mapping.

## Error codes

The codeset is a stable external contract (CLI output, JSON responses, exit
codes). One code per *category*, never per call site — prefer reusing a
category over inventing a new one. The full inventory lives in
`internal/errors/codes.go`; the exit-code mapping in `cmd/cli_errors.go`
(`ExitCodeFor`) and the README table.

## Developer guidelines

1. **Return, don't print.** Domain and infrastructure code returns errors to
   the caller. It never prints them to stdout. The CLI boundary (`main.go` →
   `RenderError`) is the only place errors are rendered.
2. **Preserve the root cause.** When wrapping, use `phelixerr.Wrapf(code, err,
   "context …")` (note the `(code, err, format)` argument order). Never use
   `%v` on a wrapped cause — callers rely on `errors.Is` / `errors.As`
   matching `*exec.ExitError`, `os.ErrNotExist/ErrPermission`, and gRPC
   `status`.
3. **Pick the most specific code.** `NOT_FOUND` for missing resources,
   `INVALID_ARGUMENT` for bad input, `CONFIGURATION_ERROR` for bad config,
   `BUILD_FAILED`/`DEPLOY_FAILED`/`ROLLBACK_FAILED` for their stages.
4. **Do not over-wrap.** Wrap when adding meaningfully different context
   (which app, which version, which stage); pass errors through unchanged
   when the code already says what happened.
5. **Keep output bounded.** Never embed command/compiler/docker output in an
   error *message*. Preserve the exit status (`*exec.ExitError`) as the
   cause; captured tool output goes on `builder.ToolError` (bounded tail) and
   is rendered only after redaction.
6. **Never leak secrets.** A value that looks like a credential (token,
   password, env value, registry cred, auth header) must never appear in an
   error, a log line, a JSON payload, or a gRPC message. `--debug` raises
   verbosity, never the secrecy bar.
7. **Errors go to stderr, once.** One headline per failure; never print an
   error and also return it.
8. **Exit codes are part of the script contract.** Same category ⇒ same exit
   code; re-check `ExitCodeFor` after changing an error value.

---

# Error Reporter — known-error enrichment layer

The CLI renderer (`cmd/cli_errors.go` → `renderCLIError`) consults an ordered
registry of *known-error resolvers* in `internal/errreport` after printing the
headline and code. Known errors gain a title, an explanation, a suggested fix,
a real command, and a docs link; everything else renders exactly as before.

## Position in the flow

```
  raw error (already structured by phelixerr, wrapped %w at classification points)
       │
       ▼
  cmd.RenderError / renderCLIError            ← the single CLI boundary
       ├── headline  = Redact(err.Error())
       ├── code      = phelixerr.CodeOf(err)   (unchanged, never remapped)
       ├── exit code = ExitCodeFor(err)        (unchanged)
       │
       ├── errreport.For(err)  →  known?
       │      ├── yes → renderReport: Title / Explanation / Suggested fix / Run / Documentation
       │      └── no  → generic hint (hintFor) as before
       │
       ├── errreport.ToolOutput(err) → captured build-tool output, redacted, bounded (8 KiB tail)
       │      (rendered for known and unknown tool failures alike)
       │
       ├── unknown & !debug & has cause → "Run with --debug for the full error chain."
       └── debug → Cause: (redacted root cause, unchanged)
```

The reporter is an **additional presentation layer, not a parallel error
system**. It does not classify, does not touch exit codes, does not replace
hints with guesses, and never hides the root cause. When a report exists it
subsumes the generic hint (which it makes redundant) — that is the only
rendering change for known errors.

## Resolvers (the registry)

`errreport.resolvers` is an ordered `[]Resolver`; the first match wins.
`errreport.Register` prepends (new/specific resolvers win). Every resolver is
cheap (runs on every rendered error) and evidence-based:

| Resolver | Fires when | Guidance produced |
|---|---|---|
| `toolchainResolver` | `IsCode(err, TOOLCHAIN_NOT_FOUND)` **and** the wrap-chain text names the tool (`"Go toolchain"`, `"golang"` / `"Rust toolchain"`, `"cargo"`, `"rustup"`) | Platform-appropriate install command from `toolchain.InstallCommand` (mirrors `detectPackageManager`: brew/pacman/dnf/yum/zypper/apk/apt/winget), official docs; `Command` stays empty when no deterministic command exists for the platform (falls back to the official download URL in the suggestion) |
| `portResolver` | `IsCode(err, PORT_UNAVAILABLE)` **and** message matches Phelix's own `port N is already in use` shape | Explanation of the conflict, best-effort listener identification (`port.FindListener`, read-only `lsof -Fpc`, omitted when unavailable), read-only per-platform inspection command (`sudo lsof -i :N` / `lsof -i :N` / `netstat -ano \| findstr :N`), explicit "Phelix never stops it for you" + `--port` alternative |
| `goModuleResolver` | `IsCode(err, BUILD_FAILED)` **and** `errors.As` finds a `*builder.ToolError` with `Tool == "go"` **and** the captured output matches one of the narrow conditions | `go mod init` (go.mod missing), manual-fix guidance (malformed go.mod / unknown directive), `go mod tidy` (missing go.sum entry, undeclared dependency), `go clean -modcache && go mod download` (checksum mismatch), version-fix guidance (unknown revision), GOPROXY guidance (lookups disabled). All docs → go.dev/ref/mod |

Detection invariants (enforced by tests):

- The structured code is the primary gate; text matching only disambiguates
  within it, and `Tool == "go"`/`"cargo"` identity is a fixed string set by
  the builder — never derived from output.
- Bare words (`error`, `failed`, `command failed`) never trigger anything;
  ordinary compiler diagnostics (`undefined: Foo`, linker failures, Rust
  `error[E0432]`) stay unknown and render raw output.
- `go mod tidy` is suggested only for the two conditions it actually fixes —
  never as a catch-all.
- Other `PORT_UNAVAILABLE` shapes (proxy bind failure, allocation race, the
  "nothing is listening" port-validation failure) are not classified.

## Captured tool output (`builder.ToolError`)

`internal/builder` records the failing tool's combined output as a **bounded
tail** (last 8 KiB, trimmed to a line boundary) on a typed
`*builder.ToolError{Tool, Output, Err}` and wraps it with the existing
`phelixerr.Wrapf(CodeBuildFailed, …)`. The chain is
`*phelixerr.Error → *ToolError → *exec.ExitError`, so:

- `errors.Is`/`errors.As` reach `*exec.ExitError` exactly as before;
- `CodeOf` still yields `BUILD_FAILED` (the wrap carries the code);
- the output never enters any error *message* — it is rendered only after
  `phelixerr.Redact`, in the bounded "Tool output (most recent lines)" section;
- matrix builds keep their own per-combination reporting and are unchanged.

## Adding a new known error

1. Add a resolver type implementing `Resolver{ Resolve(err) (Report, bool) }`
   in `internal/errreport` (or call `errreport.Register` from an init site).
2. Gate on the most specific typed signal available, in order:
   `phelixerr.IsCode` → `errors.Is`/`errors.As` → tool identity → a narrow,
   tested token/regex on chain text or captured output — never broad keywords.
3. Fill `Report{Code: <the error's existing code>, Title, Explanation,
   Suggestion, Command, DocsURL}`. `Command` must be a fixed, syntactically
   valid, platform-appropriate command the user runs themselves — empty when
   no deterministic command exists. Never interpolate untrusted error content
   into `Command`. Docs links: existing Phelix docs for Phelix-specific
   problems, official upstream docs otherwise.
4. Prefer reusing existing platform/detection helpers (`toolchain` package
   detection, `port.FindListener`) over duplicating them.
5. Tests (see below) are part of the change: known case, negative case (near
   misses must stay unknown), redaction, exit-code stability.

## Security requirements

- Every rendered report field and every captured-output line passes through
  `phelixerr.Redact` at the render boundary (normal **and** debug mode).
- Resolver inputs that reach rendered text are pre-redacted or strictly
  validated where they could embed foreign data (the port resolver extracts
  only a digit port from the message; listener names come from `lsof` and are
  redacted at render).
- Suggested commands are static strings, at most parameterized by a validated
  integer (a port). They are never executed by Phelix.
- `ToolError.Output` never flows into error messages, logs, gRPC payloads, or
  JSON reports — only into the redacted render section.

## Testing requirements

Each resolver carries: a known-case test (title/command/docs/code), a
negative-case test (near-miss codes/texts/tool identities stay unknown), and
platform-decision tests with stubbed `lookPath`/`goos`/`InstallCommand`/
`FindListener`/`runLsof` seams — no test depends on the host's installed
tools. `cmd` rendering tests pin: report sections present, headline rendered
once, exit codes unchanged, raw output present for unknown failures, no
invented fixes, and no secret leakage in normal or debug mode.

## Notes for maintainers

- `ExitCodeFor` maps `PORT_UNAVAILABLE` to exit 30 (`ExitNetwork`) per the
  documented contract; keep new network-adjacent codes consistent with the
  README table.
- Known pre-existing issue (documented, deliberately untouched here):
  `toolchain.detectPackageManager`'s apk entry drops the package name and
  uses the wrong verb, so automatic Alpine installs via that path do not
  work. The suggestion table in `toolchain/suggest.go` constructs the apk
  command correctly (`sudo apk add --no-cache go`); fixing the installer path
  itself is a separate change.
