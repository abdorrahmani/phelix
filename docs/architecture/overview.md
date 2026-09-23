# Architecture Overview

Phelix is a single-binary CLI built on [Cobra](https://github.com/spf13/cobra).
Each top-level command lives in `cmd/`, backed by domain packages in `internal/`.
The design keeps commands thin (argument parsing + orchestration) and pushes logic
into testable packages. All runtime state lives on disk under `~/.phelix/` (see
[state management](state-management.md) and the
[data-directory reference](../reference/data-directory.md)).

Requires Go 1.27 (per `go.mod`).

```text
                 ┌──────────── main.go (entry, command registration) ┐
                 │                                                     │
   cmd/  ◄──────┤  cobra commands  ──►  internal/ domain packages  ──► state on disk
 (thin)         │                                                     │
                 └─────────────────────────────────────────────────────┘
```

## The single error boundary

`main.go` registers every command and owns the **single error boundary**: cobra
runs with `SilenceUsage`/`SilenceErrors`, and both `config.Load()` failures and
`rootCmd.Execute()` failures go through `cmd.RenderError(err, cmd.Debug)` →
`os.Exit(code)`. Nothing else prints errors. Domain/infra code returns `error`;
only the `cmd` boundary renders, and only to stderr. Daemon diagnostics use
`log.Printf` / the logging package. See [error codes](../reference/error-codes.md)
and [`docs/error-architecture.md`](../error-architecture.md).

## Key design principles

- **Two-phase version promotion.** A build records a version with
  `is_current = false`. It becomes current only *after* the deploy/health check
  succeeds (`PromoteVersion`). A build that compiles but fails to start leaves the
  version on disk for inspection without ever becoming "current". See
  [state management](state-management.md).
- **Fail-safe deploys.** Blue-green/rolling never point `current` (or proxy
  traffic) at an instance that hasn't passed its health check. A failed new
  instance is killed; the active one is untouched. See [deployment](deployment.md).
- **Fail-open vs fail-closed matrices.** Native matrix builds continue past a
  failing combination and report all results (fail-open). Docker matrix *push* is
  fail-closed by default: any build failure blocks the push unless `--push-partial`
  is set. See [build system](build-system.md).
- **Atomic proxy cut-over.** The proxy reads its target from an `atomic.Value` on
  every request, so switching backends takes effect on the next request with no
  dropped connections and no per-request locking. See [deployment](deployment.md).

## Project layout

`cmd/` is thin (flag parsing + orchestration); logic lives in `internal/`.
`cmd/monitor.go` is the long-running daemon systemd supervises (`phelix monitor`)
— it restores auto-start apps, drives the health daemon, and holds the persistent
gRPC stream.

```text
main.go                  # entry point; command registration; single error boundary
config/                  # embedded config.yml (mode, api, grpcUrl) + loader
cmd/                     # CLI commands (cobra) — one area per file
├── auth/                # authentication subpackage (login/status/logout, client, session)
├── build.go rebuild.go rebuild_docker.go
├── rollback*.go         # rollback, picker, history, verify, reason, dry-run, remote
├── matrix*.go           # matrix build/exec/init/status/retry/remote
├── start.go stop.go restart.go status.go list.go log.go remove.go
├── env.go health.go proxy.go dockerize.go doctor.go init.go watch.go
├── build_report*.go webhook*.go monitor.go update.go version.go
├── wizard.go wizard_prompt.go prompts.go project_config.go
└── cli_errors.go utils.go
internal/
├── app/                 # AppManager: lifecycle, process control, apps.json state
├── builder/             # Go/Rust builders, language detection, build factory
├── buildreport/         # build-report capture + regression analysis
├── toolchain/           # detect / auto-install Go & Rust toolchains
├── deploy/              # blue-green, rolling, rollback, versioning, autoscale, deploy.json
├── proxy/               # reverse-proxy daemon + unix control socket + client
├── health/              # tiered health checks, config, background daemon, terminal UI
├── env/                 # AES-256-GCM encrypted env storage + masking
├── docker/              # Dockerfile/.dockerignore/compose generation, build, push
├── matrix/              # matrix plan, executor (bounded pool), reporters, builders
├── monitor/             # monitoring metrics collectors + command executor
├── grpc/                # gRPC client: persistent monitor stream, events, health, rollback
├── server/              # host/system metrics collection (gopsutil)
├── logs/                # logging (app + self), log rotation
├── network/             # IP utilities
├── port/                # port assignment / availability
├── project/             # phelix.yaml schema, load, validation, resolution
├── resources/           # cgroup v2 CPU/memory limits, OOM classification
├── connstate/           # monitor connection-state model
├── errreport/           # known-error explanation/fix/command/docs registry
├── errors/              # the single structured error package (phelixerr)
├── syscmd/              # command runner seam (test isolation)
├── update/              # self-update: version resolution, checksum, atomic replace
├── webhook/             # git push webhook server, queue, jobs, worktrees
└── version/             # build-time version info
```

> This layout is regenerated from the actual repository tree. The historical
> `DEVELOPMENT.md` listing predated several packages (`buildreport`, `connstate`,
> `errreport`, `port`, `project`, `resources`, `syscmd`, `update`, `webhook`) and
> stated Go 1.26.3+; the current source requires Go 1.27.

## Related

- [Identity](identity.md), [Deployment](deployment.md),
  [State management](state-management.md), [Build system](build-system.md),
  [Monitoring](monitoring.md).
- [Development setup](../development/development-setup.md) — building from source.
