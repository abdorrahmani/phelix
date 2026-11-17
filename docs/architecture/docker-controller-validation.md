# Docker Controller validation

Validation date: 2026-11-17. All changes and commits remained local. No push,
release, remote branch change or pull request was performed.

This record describes executed checks. The design is in
[docker-controller.md](docker-controller.md), and operational instructions are in
the [Controller guide](../guides/controller.md).

## Environment

The validation host used Linux amd64, Go 1.27.4, Docker Engine 29.9.3, Docker CLI
29.1.3 and Docker Compose 5.7.1. A real Docker daemon was available. Native CLI
integration tests built and ran real Go processes; Docker lifecycle checks ran
real containers. Docker unit tests retained injectable command runners.

Go commands used `GOFLAGS=-buildvcs=false` only in their validation environment.
An unrelated malformed `/tmp/.git` interfered with VCS stamping in temporary
projects. That directory and global Go configuration were left unchanged.

The public Go module proxy returned regional HTTP 403 responses for some
dependencies. The Controller image was built successfully using a temporary
localhost HTTP proxy backed by the verified host module cache:

```bash
docker build --network=host \
  --build-arg GOPROXY=http://127.0.0.1:49123 \
  -f Dockerfile.controller -t phelix-controller:local .
```

This was a validation environment workaround. The Dockerfile retains the normal
configurable Go module proxy. The image was built locally and was not published.

## Unit, integration, race and static checks

The following checks passed against the final source:

```bash
GOFLAGS=-buildvcs=false go test -json -p 1 ./...
GOFLAGS=-buildvcs=false go test -race -p 1 -count=1 ./...
GOFLAGS=-buildvcs=false go vet ./...
GOFLAGS=-buildvcs=false go build ./...
```

The final JSON test run reported 48 passing packages, 2,371 passing top-level
tests, six packages with no test files, and four skipped tests. No package or test
failed. CLI integration tests, the shared deployment engine, native process
tests, API tests, backend gRPC tests and recovery tests are included in `./...`.
The full race run also passed; it includes the final adapter ownership hardening.

Focused ownership and real CLI checks also passed with the race detector:

```bash
GOFLAGS=-buildvcs=false go test -race -count=1 ./internal/controller ./cmd \
  -run 'TestAdapterRejectsForgedOwnershipDescriptor|TestOwner|TestOwnership|TestCLIEnvironmentLoadsControllerRegistry|TestControl'
```

The source review and checks covered these behaviors:

| Area | Executed coverage |
| --- | --- |
| Controller ownership | Competing owners, lock release, inherited lease lifetime, forged descriptors, restart and identity retention |
| Persistent state | Registry retention, repeatable key/environment/session migration, conflicts and source symlink boundaries |
| Local API | Startup/shutdown, socket modes, group permissions, unauthorized peers, invalid requests and reconnect |
| Operations | Durable results, progress replay, concurrent jobs, structured errors, safe cancellation and interrupted recovery |
| CLI | API routing for every management family; real CLI state, environment, secrets, security, authorization, plans, sessions, MCP and workspace checks |
| Runtime endpoints | Native loopback, Docker host loopback, bridge IPs, unique rolling endpoints, metadata refresh and failed discovery |
| Deployment safety | Existing blue-green, rolling, canary, progressive, health, drain, rollback, secret transaction and recovery tests |
| Docker ownership | Stable labels, foreign-container rejection and positive identity checks |
| Monitoring | Stats parsing, missing containers, daemon errors, cache freshness, native sampling and backend stream/log batching |

### Tests skipped in the ordinary suite

These skips are reported separately from their successful dedicated executions:

| Test | Ordinary-suite skip reason | Dedicated result |
| --- | --- | --- |
| `internal/deploy.TestDockerSecretLifecycle` | Requires `PHELIX_TEST_DOCKER=1` | Passed with the real Docker daemon |
| `internal/matrix.TestBuildImageSimple_NoDocker` | Docker is installed | Passed with an isolated empty PATH |
| `internal/matrix.TestBuildImageSimple_BuildArgsNotEchoedInError` | Docker is installed; avoids an unintended real build | Passed with an isolated empty PATH |
| `internal/toolchain.TestEnsureTool_DeclinedPrompt_ReturnsErrorWithManualInstructions` | Cargo is installed | Passed with an isolated empty PATH |

The dedicated Docker secret test used:

```bash
PHELIX_TEST_DOCKER=1 GOFLAGS=-buildvcs=false \
  go test -count=1 ./internal/deploy -run '^TestDockerSecretLifecycle$'
```

It verified multiline credential integrity, redacted Docker log drains and
transaction recovery.

Missing-tool checks compiled race-enabled test binaries before changing PATH:

```bash
GOFLAGS=-buildvcs=false go test -race -c \
  -o /tmp/phelix-matrix-missing-tool.test ./internal/matrix
PATH=/nonexistent /tmp/phelix-matrix-missing-tool.test -test.v \
  -test.run '^TestBuildImageSimple_(NoDocker|BuildArgsNotEchoedInError)$'
GOFLAGS=-buildvcs=false go test -race -c \
  -o /tmp/phelix-toolchain-missing-tool.test ./internal/toolchain
PATH=/nonexistent /tmp/phelix-toolchain-missing-tool.test -test.v \
  -test.run '^TestEnsureTool_DeclinedPrompt_ReturnsErrorWithManualInstructions$'
```

No installed tool was removed and no existing test was weakened.

## Real Docker lifecycle

The isolated lifecycle validation used temporary test orchestration that is not
distributed with the repository. The complete
[Host Mode](../../examples/docker-host-controller/README.md) and
[Container Mode](../../examples/docker-container-controller/README.md) examples
document how to reproduce the application management flows with the installed CLI.

The final run passed in both Host Mode and bridge-network Container Mode:

1. Start Controller and connect the host CLI through the Unix socket.
2. Build and deploy two independent applications concurrently.
3. Verify readiness and traffic for both applications; remove one in isolation.
4. Deploy blue-green, rolling with two replicas, canary and progressive versions.
5. Continuously send HTTP traffic during each deployment and rollback; observe
   no failed requests, and verify the expected version after promotion.
6. Roll back each strategy to the recorded v1 image.
7. Read application logs, runtime information and sampled Docker metrics.
8. Verify Controller/app identity labels and Host Mode versus Container Mode
   port publication.
9. Restart the Host Mode Controller or remove/recreate the Controller container.
10. Verify stable Agent ID and application ID, retained application container
    IDs, state recovery and restored proxy traffic.
11. Stop, start, restart and remove the application through the API-backed CLI.
12. Verify that a test-owned fixture carrying another installation's Controller
    label remains running after Phelix removal.

Validation used unique temporary state, names, networks and resources. Test
application containers/images, Controllers and networks were cleaned up, while
temporary logs/state were retained for inspection. Pre-existing host services
were left untouched.

One earlier Docker run under concurrent full-suite race-test load correctly
rejected a canary latency spike. The final isolated run passed. No verification
threshold was relaxed and the rejection was not counted as a successful run.

## Complete example validation

Both supplied examples were run with their own application Dockerfiles and
Compose Redis/PostgreSQL services:

- [Host Mode](../../examples/docker-host-controller/README.md)
- [Container Mode](../../examples/docker-container-controller/README.md)

Executed checks included setup, Compose infrastructure readiness, application
build, encrypted environment configuration, deployment, `/health`, proxy traffic,
logs, Docker metrics, version upgrade, rollback, start/stop/restart and removal.
The Container example also passed forced Controller recreation with persistent
identity and recovery. The sample source was restored to v1 afterward.

Example Compose configuration, shell setup scripts and installer syntax were
validated. Both example Compose projects were brought down after validation
without deleting named volumes. Unrelated pre-existing `Phelix_*` services were
left running. The READMEs contain the complete terminal commands and cleanup
instructions; they describe the tested mounts and network topology.

## Verification artifacts

The final session retained these host-local artifacts for manual inspection.
They are temporary evidence, not runtime dependencies or committed secrets:

- `/tmp/phelix-controller-unit-final.jsonl`
- `/tmp/phelix-controller-race-final.log`
- `/tmp/phelix-controller-ownership-final.log`
- `/tmp/phelix-controller-vet-complete.log`
- `/tmp/phelix-controller-build-complete.log`
- `/tmp/phelix-controller-image-final-owner.log`
- `/tmp/phelix-controller-docker-final-pass.log`
- `/tmp/phelix-controller-docker-secret-final.log`
- `/tmp/phelix-controller-missing-docker.log`
- `/tmp/phelix-controller-missing-cargo.log`
- `/tmp/phelix-host-integration-k3i6u7lg`
- `/tmp/phelix-container-integration-5mzsfjpx`

## Operational limits and untested environments

- The local Control API and private adapters are supported on Linux. Windows and
  macOS Controller transports were not implemented or validated.
- State must reside on a local filesystem with reliable OS file locking. Shared
  network-filesystem ownership is unsupported.
- The Controller owns its proxy. A Controller outage interrupts traffic until
  restart recovery, even while application containers keep running. The tests
  establish uninterrupted traffic during deployment and rollback, not during a
  Controller outage.
- Container startup validates Engine metadata using the default Docker container
  hostname. Do not override that hostname in the supplied topology.
- Docker socket access grants administrative power over the host Docker
  environment. A read-only mount does not restrict Engine mutations. The supplied
  Controller runs as root, and local API access is restricted by filesystem
  permissions and numeric peer UID authorization.
- Docker metrics can return `Unavailable` before the first background sample or
  after a daemon failure. Clients should retry rather than interpret this as zero
  resource usage.
- Management adapters have no interactive terminal. Provide application names
  or IDs explicitly; the local wizard delegates its chosen operations to the API.
- Operation results persist, but progress is an in-memory bounded replay buffer
  and does not survive Controller restart. Completed operation records are capped
  at 10,000; archive terminal records with the Controller stopped before reaching
  the cap. Explicit cancellation is available only for supported safe operations.
- The typed history query exposes the existing rollback history. Retained version
  history, deployment reports and application operation inspection remain
  available through their API-backed commands; no historical data was reset.
- Live external dashboard authentication was not exercised. Existing backend
  gRPC unit/integration/race coverage passed, including reconnect and log batching.
- Rootless Docker, custom user namespaces, IPv6-only Docker networks, different
  CPU architectures and an internet-facing authenticated remote management
  transport were not validated. No public management listener was added.
