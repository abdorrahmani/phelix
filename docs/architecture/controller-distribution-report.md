# Phelix Controller distribution implementation report

## A. Executive summary

Phelix now has an official Docker Controller distribution and a first-class
Container Mode installer with an optional host CLI. Host Mode continues to use
systemd and supports native and Docker applications. Container Mode manages
Docker service applications through the host Docker Engine.

Both modes reuse the existing Controller, local management API, deployment
engine, runtime interfaces, proxy, application registry, secrets and deployment
history. Existing status and remove selectors remain supported. No second
Controller is installed in application images.

Completed stages have local Conventional Commits. Nothing was pushed, released
or published, and no PR was created. A separate PR draft is provided for manual
creation after review and testing.

## B. Final architecture

The diagram and original audit are in
[controller-modes.md](controller-modes.md).

In Host Mode, systemd runs the Controller directly. The host CLI reaches its
private Unix socket. Native processes use existing host launchers, while Docker
applications use Engine-built images and loopback ports. The Controller owns the
proxy and restores application routes after restart.

In Container Mode, Compose runs one Controller on a configured shared Docker
network. The optional host CLI and the CLI inside that container both reach the
same local management API. State and the socket directory are mounted outside
the container; projects have explicit host-to-container mappings. Docker Engine
builds application images from those mounted files. Health checks and the proxy
use exact application container addresses. Application Dockerfiles provide the
build environment, including language compilers.

The existing outbound monitoring/backend gRPC protocol is separate from the
local management API. Existing command handlers execute as private Controller
adapters, preserving their behavior without sharing process-global flags or
working directories between concurrent operations.

## C. Work by phase

| Phase | Result and reason | Main files |
| --- | --- | --- |
| 0 | Audited real execution paths, existing support, migration risks, source access, proxy networking and release limits before implementation. | `docs/architecture/controller-modes.md` |
| 1 | Retained one shared initialization path and persistent file ownership lock. Startup and cleanup errors propagate. Native recovery and launch restrictions protect Host Mode applications during migration. | `cmd/controller.go`, `internal/controller/controller.go`, native launchers and mode tests |
| 2 | Retained the versioned local API. Readiness starts after service initialization, shutdown allows a bounded drain, and stale sockets require safe ownership and permissions. | `cmd/monitor.go`, `internal/controlapi/` |
| 3 | Preserved management command routing and interactive selectors. Installed host clients discover the shared endpoint; explicit settings win. Command deadlines reach operations. | `cmd/control_client.go`, `cmd/control_mcp.go`, `internal/controller/config.go` |
| 4 | Rejected overlapping roots and escaping source access. Named deployments resolve registered projects, and relative source overrides map from the caller's directory. Existing Docker build/cache behavior remains shared. | `cmd/control_service.go`, source configuration and request tests |
| 5 | Retained runtime-aware endpoints and Docker statistics. Endpoint recovery honors cancellation and rejects networks the Controller cannot use. | `internal/deploy/endpoint.go` and endpoint tests |
| 6 | Added pinned multi-stage image inputs, release metadata, Container Mode defaults, health check and versioned GHCR publishing. Each architecture must pass native tests and image/runtime checks before its manifest is published. | `Dockerfile.controller`, `.github/workflows/release.yml`, image check script |
| 7 | Added `--docker`, optional `--with-cli`, directory/network options, explicit proxy ports, prerequisites, generated Compose, readiness checks and conservative installation safeguards. | `install.sh`, installer tests, Container installation guide |
| 8 | Exercised restart, forced recreation, socket recovery, stable identity and interrupted operation records. Image self-update and host systemd restart are rejected in Container Mode with actionable instructions. | Image recovery script, update/restart handlers and tests |
| 9 | Changed the main example to pull the official image, separated development builds, and packaged standalone examples with checksums. Updated installation, operation, security and migration documentation. | Examples, release packaging/publishing scripts and guides |
| 10 | Added reusable live Docker validation for both modes and every supported strategy, plus release validation gates. Results and infrastructure limits are recorded below. | `.github/scripts/test-controller-docker.sh`, this report and PR draft |

Final validation found and fixed another recovery issue: state reload already
reported Docker applications as running, so the existing save comparison missed
changes to automatic recovery intent. The corrected comparison also considers
intent, port and start time. Its regression test proves intent survives reload.
Live recreation checks then verify that routes return while app identity and
versions remain intact.

Native builds are now rejected before toolchain setup. Container installation
also refuses an enabled host service, preventing an inactive host Controller
from returning alongside the container after reboot.

No state schema, encryption format or deployment strategy semantics were
replaced. The existing process-safe state lease and Docker ownership labels
remain the ownership boundary. Separate deliberately configured stores have
independent identities; production deployments should use one Controller store
per managed host.

## D. Files changed

The file inventory below covers the distribution commits after `cde3fb7`.
Existing environment-command, development-configuration and local tooling edits
were preserved and excluded from these commits. No files were removed.

### Created

- `.github/scripts/build-examples.sh`
- `.github/scripts/check-controller-image.sh`
- `cmd/control_source_test.go`
- `cmd/controller_modes_test.go`
- `cmd/controller_update_test.go`
- `controller_examples_test.go`
- `controller_image_test.go`
- `docs/architecture/controller-modes.md`
- `docs/guides/container-installation.md`
- `examples/docker-container-controller/compose.build.yaml`
- `installer_container_test.go`
- `internal/app/runtime_mode_test.go`
- `internal/controlapi/socket_lifecycle_test.go`
- `internal/controller/client_config_test.go`
- `internal/controller/source_config_test.go`
- `internal/deploy/runtime_mode_test.go`
- `.github/scripts/test-controller-docker.sh`
- `docs/architecture/controller-distribution-report.md`
- `docs/architecture/controller-distribution-pr.md`

### Modified

- `.github/scripts/publish-github.sh`
- `.github/scripts/release-tag.sh`
- `.github/workflows/release.yml`
- `Dockerfile.controller`
- `README.md`
- `cmd/app_runtime_test.go`
- `cmd/build.go`
- `cmd/control_client.go`
- `cmd/control_mcp.go`
- `cmd/control_service.go`
- `cmd/controller.go`
- `cmd/lifecycle.go`
- `cmd/monitor.go`
- `cmd/update.go`
- `docs/getting-started/installation.md`
- `docs/guides/controller.md`
- `examples/docker-container-controller/.env.example`
- `examples/docker-container-controller/README.md`
- `examples/docker-container-controller/compose.yaml`
- `examples/docker-container-controller/setup.sh`
- `examples/docker-host-controller/README.md`
- `install.sh`
- `internal/app/process.go`
- `internal/controlapi/client.go`
- `internal/controlapi/credentials_linux.go`
- `internal/controlapi/credentials_other.go`
- `internal/controlapi/server.go`
- `internal/controller/config.go`
- `internal/controller/controller.go`
- `internal/deploy/endpoint.go`
- `internal/deploy/endpoint_controller_test.go`
- `internal/deploy/instance.go`
- `internal/deploy/worker_launch.go`
- `internal/update/restart.go`
- `internal/update/restart_test.go`

## E. Installation and usage

After the reviewed release is published, install Host Mode with:

```bash
curl -fsSL https://phelix.anophel.com/install.sh | bash
phelix version
sudo systemctl status phelix
sudo systemctl restart phelix
```

Host Mode native builds need their language toolchains. Docker builds use the
application Dockerfile's builder stage. Docker is optional for native-only hosts.

For a fresh Container Mode installation:

```bash
curl -fsSL https://phelix.anophel.com/install.sh | bash -s -- --docker --with-cli --proxy-port 8080
phelix controller health
phelix controller info
```

Omit `--with-cli` when operating inside the Controller. To pull a pinned image
using the currently published stable version:

```bash
PHELIX_VERSION="$(curl -fsSL https://phelix.anophel.com/releases/latest/version)"
PHELIX_IMAGE_TAG="${PHELIX_VERSION//+/_}"
docker pull "ghcr.io/abdorrahmani/phelix:$PHELIX_IMAGE_TAG"
docker compose -f /etc/phelix/compose.yaml up -d --wait --wait-timeout 90 phelix
```

Prepare `phelix.yaml`, Dockerfile and `.dockerignore` under the configured project
root before applying read-only access. A complete project is supplied in each
example. From `/srv/phelix/projects/my-api`:

```bash
phelix build my-api
phelix deploy my-api
phelix status my-api
phelix logs my-api --no-follow --lines=20
phelix controller metrics my-api
phelix rollback my-api --to=v1 --reason=manual --verify=5s
```

Container CLI equivalents:

```bash
docker compose -f /etc/phelix/compose.yaml exec -T -w /projects/my-api phelix phelix build my-api
docker compose -f /etc/phelix/compose.yaml exec -T phelix phelix deploy my-api
docker compose -f /etc/phelix/compose.yaml exec phelix phelix status
docker compose -f /etc/phelix/compose.yaml exec phelix phelix remove
```

For restart and upgrade, keep the same state, socket, configuration and project
mounts:

```bash
docker compose -f /etc/phelix/compose.yaml restart phelix
docker compose -f /etc/phelix/compose.yaml stop phelix
sudo sh -c 'umask 077; tar -C /var/lib -czf /root/phelix-state-backup.tar.gz phelix'
sudo cp -p /etc/phelix/compose.yaml /etc/phelix/compose.yaml.before-upgrade
```

Edit the Compose image to the chosen published version, then:

```bash
docker compose -f /etc/phelix/compose.yaml pull phelix
docker compose -f /etc/phelix/compose.yaml up -d --no-deps --wait --wait-timeout 90 phelix
docker compose -f /etc/phelix/compose.yaml exec -T phelix phelix controller info
docker compose -f /etc/phelix/compose.yaml exec -T phelix phelix list
curl --fail http://127.0.0.1:8080/health
```

Verify identity, versions, secrets, status and traffic against the pre-upgrade
record before removing the old image or backup. Update the optional host CLI
with `--no-service`. The image rejects binary self-update. Reverting an image
requires a compatible state backup if its schema changed.

Migration is documented in
[Container installation](../guides/container-installation.md). Record identities,
runtimes, operations, paths and networks; stop native apps and the host service;
disable its reboot startup; back up the entire store; retain original absolute
paths with reviewed mounts; then start the container and verify identity and
traffic. The installer refuses automatic adoption. The original store, unit and
binary remain available for rollback.

## F. Running the examples

The release workflow attaches two standalone archives and their SHA-256 files.
They contain the application, Dockerfile, project configuration, Compose,
setup script, instructions and release version. Generated credentials and state
are excluded.

For the container archive:

```bash
curl -fLO "https://github.com/abdorrahmani/phelix/releases/download/v$PHELIX_VERSION/docker-container-controller.tar.gz"
curl -fLO "https://github.com/abdorrahmani/phelix/releases/download/v$PHELIX_VERSION/docker-container-controller.tar.gz.sha256"
sha256sum -c docker-container-controller.tar.gz.sha256
tar -xzf docker-container-controller.tar.gz
cd docker-container-controller
./setup.sh
docker compose pull phelix
docker compose up -d --wait
docker compose exec -T -w /workspace/app phelix phelix build container-example
docker compose exec -T phelix phelix status container-example
curl --fail http://127.0.0.1:8080/health
```

For Host Mode, download and extract `docker-host-controller.tar.gz` and its
checksum from the same release, then:

```bash
cd docker-host-controller
./setup.sh
docker compose up -d --wait
export PHELIX_DATA_DIR="$PWD/state"
export PHELIX_CONTROLLER_CONFIG="$PWD/controller.yaml"
export PHELIX_CONTROL_SOCKET="$PWD/state/control.sock"
phelix controller
```

Follow its README in another terminal to build `host-example`, set infrastructure
addresses, deploy, inspect, roll back and clean up. Compose owns Redis and
PostgreSQL. Phelix owns application containers. Neither example requires a
Phelix source checkout after installation. A separate `compose.build.yaml` in
the repository is reserved for development builds.

## G. Validation results

Validation ran locally on Linux amd64 with Go 1.27.4, Docker Engine 29.9.3,
Compose 5.7.1 and Buildx 0.39.0.

The final full suite passed with 50 passing packages, 3,684 passing test results
including subtests, six explicitly skipped tests and no failures:

```bash
go test -json -p 1 -count=1 ./...
```

An earlier parallel run hit the existing webhook test
`TestWebhookHandler_ResponseDoesNotWaitForRebuild`: its assertion observed the
queue before the asynchronous job callback. That test passed on retry, and the
subsequent complete serial suite passed. No unrelated webhook code was changed.

Core race and static checks passed:

```bash
go test -race -count=1 ./internal/controller ./internal/controlapi ./internal/deploy ./internal/proxy ./internal/monitor ./cmd
go vet ./...
go build ./...
bash -n install.sh .github/scripts/*.sh examples/docker-container-controller/setup.sh examples/docker-host-controller/setup.sh
```

Focused suites checked native mode restrictions, ownership, identity retention,
storage conflicts, socket permissions and stale recovery, unauthorized API
peers, operation progress/cancellation, CLI routing/selectors, mounted source
mapping, Docker endpoints, installer failure handling and release packaging.
The generated installer Compose file was also accepted by real Docker Compose.
Installer download/pull/start failures were exercised with isolated command
fixtures; no official unpublished image was claimed to have been installed.

A Linux amd64 Controller image built from committed source extracted with
`git archive`. It contained production backend configuration, independently of
the existing local development settings. The image passed architecture/version,
startup, health, API, graceful shutdown, restart, forced recreation, identity,
native-build rejection and interrupted-record checks:

```bash
bash .github/scripts/check-controller-image.sh phelix-controller:review 0.0.0-dev amd64
```

Real Engine runs passed all four strategies in both modes, including continuous
traffic during deployment, correct runtime endpoints, positive host PIDs,
status/uptime, logs, Engine metrics, rollback to v1, retained identity, route
recovery, stop/start/restart and removal. An unhealthy blue-green candidate was
rejected while the previous version continued serving:

```bash
go build -o /tmp/phelix-controller-review-cli .
bash .github/scripts/test-controller-docker.sh phelix-controller:review /tmp/phelix-controller-review-cli container
bash .github/scripts/test-controller-docker.sh phelix-controller:review /tmp/phelix-controller-review-cli host
PHELIX_TEST_DOCKER=1 go test ./internal/deploy -run '^TestDockerSecretLifecycle$' -count=1
```

The final Container Mode run also removed the Controller during an actual Docker
build. After recreation on the same mounts, the operation was reported as
`INTERRUPTED` and the previous v1 application continued serving. The complete
script then passed all four strategies with exit status 0. Its final log is
`/tmp/phelix-controller-container-strategies-final.log`; the Host Mode log is
`/tmp/phelix-controller-host-strategies-final.log`.

The Docker secret lifecycle test separately passed with the real Engine,
including multiline credentials and log redaction. Native CLI integration and
existing backend integration tests are included in the full suite.

Three environment-dependent tests skipped by the ordinary suite were separately
run with an isolated executable search path. All three passed without skips:

```bash
go test -c -o /tmp/phelix-controller-matrix-fallback.test ./internal/matrix
PATH=/nonexistent /tmp/phelix-controller-matrix-fallback.test -test.v -test.run='^TestBuildImageSimple_(NoDocker|BuildArgsNotEchoedInError)$'
go test -c -o /tmp/phelix-controller-toolchain-fallback.test ./internal/toolchain
PATH=/nonexistent /tmp/phelix-controller-toolchain-fallback.test -test.v -test.run='^TestEnsureTool_DeclinedPrompt_ReturnsErrorWithManualInstructions$'
```

Arm64 executable compilation passed and inspection reported an AArch64 ELF:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /tmp/phelix-controller-arm64 .
file /tmp/phelix-controller-arm64
```

Local Buildx offered only amd64 execution. Arm64 image execution was not claimed.
The release workflow uses native amd64 and arm64 runners and gates publication
on each runner's Go suite, image build, architecture, startup, health and live
strategy checks. The combined image manifest waits for both jobs.

### Local registry and module access

Docker Hub returned HTTP 403 locally. The configured mirror supplied the same
pinned content digests. A temporary loopback HTTP server served the verified Go
module download cache because of regional module access limits. No registry or
Go global configuration was changed. The normal Dockerfile defaults remain
Docker Hub and the public Go proxy.

The local build used:

```bash
python3 -m http.server 49124 --bind 127.0.0.1 --directory /home/anophel/go/pkg/mod/cache/download
docker build --network=host -f Dockerfile.controller -t phelix-controller:review \
  --build-arg GOPROXY=http://127.0.0.1:49124 \
  --build-arg GO_IMAGE=docker.arvancloud.ir/library/golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 \
  --build-arg RUNTIME_IMAGE=docker.arvancloud.ir/library/docker:29-cli@sha256:34507eb5069d8be82c95dd3b6987d4250fd5fee49b82e2509b7d7a7030740d6d \
  --build-arg VERSION=0.0.0-dev \
  --build-arg REVISION=897e328 /tmp/phelix-controller-committed
```

Run the HTTP server in a separate terminal, with a module cache and port suited
to your machine. The temporary source directory was populated with `git archive`
to exclude existing unrelated edits. The image is local and was not pushed.

### Skips and remaining infrastructure validation

| Check | Result or reason |
| --- | --- |
| Docker secret test in the ordinary suite | Requires `PHELIX_TEST_DOCKER=1`; separately passed against the real Engine. |
| Two matrix tests for missing Docker/error fallback | Ordinary suite skips when Docker is installed; both separately passed with an isolated search path. |
| Toolchain prompt fallback test | Ordinary suite skips while Cargo is installed; separately passed with an isolated search path. |
| Official external SBOM schema validation | Existing optional test requires `PHELIX_VALIDATE_STANDARDS=1`; not enabled for this change. |
| Real Cosign signing/trust | Existing optional test requires opt-in and Cosign; Cosign was unavailable. |
| Arm64 image startup and strategies | No native arm64 execution or emulation on this host; enforced in release CI. |
| Host reboot and Docker daemon restart | Not performed on the user's host with existing running services; use dedicated infrastructure. |
| Actual systemd installation/restart | Existing unit generation retained; real host service was not changed. Foreground Host Mode lifecycle was exercised. |
| Production state migration | Documented reviewed workflow; the user's existing installation was not migrated. |
| GHCR publishing, GitHub releases and image scanning | Workflow implemented and inspected; no publication or hosted workflow run performed. |
| Non-root Controller operation | Permission requirements documented; default root setup was exercised. |

## H. Security and operational limits

The API is local gRPC over a Unix socket, with restrictive filesystem permissions
and Linux peer authorization. Group permissions alone do not authorize a caller.
Unsafe stale sockets are refused. Explicit socket settings and existing host
configuration take precedence over installed endpoint discovery.

The default container runs as root to access the Engine socket and assign socket
ownership. The Docker socket grants administrative power over the host; mounting
it read-only would not restrict Docker operations. No privileged container,
Docker-in-Docker, host-network shortcut or public management port was added.

State, keys and encrypted secrets live on persistent mounts. Project roots are
explicitly mapped, normalized and checked for escaping symlinks or traversal;
overlapping roots are rejected. Source mounts are read-only for ordinary builds.
Generating project files requires a deliberate writable preparation step.

Application endpoints are unique per instance. Compose owns infrastructure and
network setup; Phelix owns application lifecycle. The installer publishes only
requested proxy ports on loopback. External access requires reviewed bindings
and firewall rules.

Controller upgrades briefly interrupt its in-process proxy. App containers can
stay running while the Controller is unavailable. Container Mode supports Docker
service apps; native processes, workers and jobs retain their existing Host Mode
requirements. Remote systemd restart is unavailable in Container Mode. Backups
must include the full store and be taken with the Controller stopped.

## I. Local commits

| Phase | Commit | Message |
| --- | --- | --- |
| 0 | `31a7c43` | docs(architecture): document dual controller execution modes |
| 1 | `f584e6b` | refactor(controller): unify controller lifecycle across execution modes |
| 2 | `2009f81` | feat(api): harden local controller management lifecycle |
| 3 | `342babf` | refactor(cli): discover installed controller endpoint automatically |
| 4 | `99c7121` | feat(build): validate mounted project roots and named deployments |
| 5 | `cf9c434` | feat(runtime): validate endpoint recovery against controller networking |
| 6 | `ca69dac` | feat(docker): add official phelix controller image |
| 7 | `e9bdcab` | feat(installer): support docker controller installation |
| 8 | `24ceb24` | feat(controller): support container lifecycle recovery |
| 9 | `153cc29` | docs(examples): add published controller installation workflows |
| 10, recovery correction | `d0fd047` | fix(controller): persist recovery intent for observed docker apps |
| 10, source correction | `b91c799` | fix(build): map relative project source overrides |
| 10, runtime correction | `1f51660` | fix(controller): reject native builds before toolchain setup |
| 10, installer correction | `897e328` | fix(installer): prevent controllers from conflicting after reboot |
| 10, final validation | Recorded by `git log -1` after this report is committed | test(controller): validate host and container execution modes |

Every commit is local and has a single-line Conventional Commits message. The
final commit contains this report, so its hash is recorded by branch history and
the delivery message rather than embedded in its own contents.

## J. Remaining manual actions

Review and test the local branch, then create the PR using
[controller-distribution-pr.md](controller-distribution-pr.md). Publishing still
requires the reviewed release tag and successful hosted amd64/arm64 validation.
No public image availability is implied before that workflow runs.

Use dedicated infrastructure to verify Docker daemon restart and host reboot.
Review production migration paths, identities, networks, keys and snapshots
before switching an existing installation. Preserve the original backup and
service configuration until the new Controller has been verified.

## K. Manual review guide

Review `Dockerfile.controller` and `.github/workflows/release.yml` first for
pinned inputs, production configuration, release metadata, architecture gates,
credentials and tag policy. Review `install.sh` next for directory permissions,
existing-installation safeguards, Compose generation and endpoint discovery.

For behavior, trace `cmd/controller.go` into the shared monitor lifecycle,
`internal/controlapi/server.go` and `cmd/control_service.go`. Follow a host CLI
and an inside-container CLI request through `cmd/control_client.go` and the
private adapter. Verify that the persistent owner and identity are shared.

Check `internal/controller/config.go` for source mapping and explicit setting
precedence. Check `internal/deploy/endpoint.go`, native launch guards and
`cmd/lifecycle.go` for mode restrictions, endpoint recovery and saved automatic
recovery intent. Then run both live validation scripts and the provided examples.

The user's backend project, active production containers, remote settings and
unrelated local edits were not changed as part of this implementation.
