# PR draft

## Title

feat(controller): ship official Docker distribution with dual installation modes

## Description

Phelix can now be installed as a versioned Docker Controller image with an
optional host CLI. Host Mode retains systemd and native applications. Both modes
use the existing local gRPC API, deployment engine, persistent state and runtime
interfaces.

The installer creates explicit state, socket, source and network configuration,
checks readiness, and refuses conflicting services or existing state. The host
CLI discovers its installed endpoint automatically. Project mounts support
read-only builds with validated path mapping. Docker endpoints remain specific
to each instance, including after recovery.

This also fixes a recovery failure where refreshed Docker status prevented
saving automatic recovery intent, leaving proxy routes absent after Controller
recreation. Native builds and launches are rejected in Container Mode, and image
upgrades use Compose with a documented backup and migration workflow.

The release workflow retains CLI artifacts and adds pinned-base Controller
images, OCI metadata, image scanning, native amd64/arm64 validation gates and
standalone example archives. Stable releases update `latest`; beta releases do
not.

Validation covers the full Go suite, race checks, API socket security, source
mapping, installer failures, actual Docker builds, all four deployment
strategies, unhealthy candidate rejection, rollback, logs, metrics, lifecycle
operations and identity preservation. See
`docs/architecture/controller-distribution-report.md` for exact results and manual
verification commands. Container recovery was also checked after removing the
Controller during a real Docker build: the operation became interrupted and the
previous application version continued serving after recreation.

Local image execution was validated on Linux amd64. Arm64 executable compilation
was checked; its image startup and runtime checks remain gated by the native
arm64 release job. Host reboot, Docker daemon restart and actual GHCR publication
were not performed. Controller restart briefly interrupts its in-process proxy.
Migration of production state remains a reviewed backup-and-restore operation.
