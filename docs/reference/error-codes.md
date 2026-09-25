# Error Handling & Codes

## How errors are rendered

- Errors are rendered **once, on stderr**, with a short code, the message, and an
  actionable hint. Successful output stays on stdout and is never polluted with
  error text.
- Authentication errors (e.g. an expired session during `phelix auth status`)
  prompt you to run `phelix auth login`. Build/run commands never require it.
- Build errors show the failing stage and a hint, and the wrapped root-cause
  chain (e.g. the failing cargo/go command with its exit status and the useful
  tail of its diagnostics) is rendered on stderr — no `--debug` required.
- Connection errors are logged and retried automatically.
- Server communication errors are handled gracefully, without crashing the CLI.
- **Root causes are preserved.** Wrapped errors keep the underlying cause
  reachable via `errors.Is` / `errors.As`, so `os.IsNotExist`, `exec.ExitError`,
  and gRPC `status.Code` still work on the cause.
- **`--debug`**: passes the flag to any command to render the complete error chain
  on stderr (normal mode bounds the chain to the first few wrapped layers;
  `--debug` lifts that cap). It never discloses secrets — every rendered string is
  run through `phelixerr.Redact` in both normal and debug mode.

## Error Reporter (known errors)

For a small registry of **known, recognizable problems**, Phelix adds an
explanation, a concrete suggested fix, a real command you can run yourself, and a
documentation link on top of the usual error line. The error code, exit code, and
root-cause chain are unchanged — the reporter only adds context.

Currently recognized:

| Problem | Example guidance |
|---|---|
| Missing **Go** toolchain (`TOOLCHAIN_NOT_FOUND`) | platform-appropriate install command (via Phelix's package-manager detection: `sudo apt-get …` / `sudo dnf …` / `brew install go` / `winget …`), docs at go.dev/doc/install |
| Missing **Rust** toolchain (`TOOLCHAIN_NOT_FOUND`) | rustup install command, docs at rust-lang.org/tools/install |
| **Port already in use** (`PORT_UNAVAILABLE`) | names the listening process and PID when the OS can tell (read-only `lsof` lookup), a read-only inspection command, and the `--port` alternative. Phelix **never** stops the process for you |
| **go.mod problems** (`BUILD_FAILED`) | `go mod init` for a missing go.mod, `go mod tidy` for go.sum drift or undeclared dependencies, manual-fix guidance for malformed go.mod, cache-clear guidance for checksum mismatches — each only for the specific condition it matches |

Example (`go.mod` with an unknown directive):

```text
Error: build failed
  Code: BUILD_FAILED

  Invalid go.mod

  go.mod could not be parsed — it likely contains a syntax error or an
  unsupported directive at the line named in the go tool output below.

  Suggested fix:
  Fix the reported line in go.mod, then build again. Once the file parses,
  go mod edit -fmt reformats it.
  Documentation:
    https://go.dev/ref/mod

  Tool output (most recent lines):
    go: errors parsing go.mod:
    go.mod:5: unknown directive: toolchainx
```

**Unknown errors stay unknown.** Most compiler and system failures have no smart
suggestion — and none is invented for them. An unrecognized error keeps its code,
shows the raw captured tool output (if any) and the root cause via `--debug`,
exactly as before. Only deterministic, evidence-based signals (structured error
codes, `errors.Is`/`errors.As`, tool identity, and narrowly matched, tested
tool-output conditions) trigger a suggestion; generic words like "error" or
"failed" never do.

Suggested commands are **informational only** — Phelix never executes them for
you, and every rendered string (including captured compiler output) passes through
the same secret redactor as the rest of the error path.

## Related

- [Exit codes](exit-codes.md) — the exit-code table this maps onto.
- [`docs/error-architecture.md`](../error-architecture.md) — the full error
  architecture, code inventory, and developer rules.
- [Error-handling rules for contributors](../development/development-setup.md).
