# Operating the Phelix Controller

Phelix has one Controller for a managed host. The CLI submits management requests
over a private Unix socket and waits for their terminal result. The Controller
uses the existing application services and deployment engine for native processes
and Docker containers. The outbound dashboard gRPC connection remains separate.

## Start and connect

Install Phelix using the one-line installer:

```bash
curl -fsSL https://phelix.anophel.com/install.sh | bash
```

The installer starts `phelix controller` through systemd on Linux. Run it as the
service account that owns your existing state. For a manual foreground instance,
stop the installed service first. Use a release with Controller support; the
installer downloads published binaries, not unpublished changes in a checkout.

The Controller is the long-running service that owns local application management,
deployments, state locking, recovery and the proxy. Monitoring is one of its
responsibilities: it collects metrics and can maintain the existing outbound
backend connection for watched applications. Local management works without
backend monitoring or dashboard authentication. `phelix monitor` is a
compatibility alias for `phelix controller`, so both commands start the same
service. Do not run them as separate services.

## Why the Controller exists

The earlier monitor already handled recovery, health checks, background jobs and
backend reporting. The Controller reuses that lifecycle and adds one owner for
local state, a private management API, supervised operations and proxy ownership.
This lets a CLI on the host manage applications through a Controller running
inside Docker, where source paths and application network addresses differ from
the host. The deployment engine and backend monitoring protocol are reused.

The existing monitor could have been extended under its old name. `controller`
names the broader management role; `monitor` remains a compatibility entry point
to that same service. See [Monitoring](monitoring.md#enable-backend-reporting) to
enable backend reporting.

## Foreground startup

```bash
export PHELIX_DATA_DIR=/absolute/persistent/phelix
export PHELIX_CONTROLLER_CONFIG="$PHELIX_DATA_DIR/controller.yaml"
phelix controller
```

The CLI defaults to `$PHELIX_DATA_DIR/control.sock`, or the existing data
directory when that variable is unset. Override discovery with
`PHELIX_CONTROL_SOCKET=/absolute/path/control.sock` on the CLI and Controller.
Unix socket paths must be at most 100 bytes. There is no TCP management listener.

```bash
phelix controller health
phelix controller info
phelix list
phelix status myapp
```

Running `phelix status` or `phelix remove` without an app argument in a terminal
opens an app selector. The CLI reads the Controller's application list and sends
the selected app ID to the Controller. Names and IDs appear together so apps
with the same name remain distinguishable. Press Ctrl+C to cancel. Scripts
without terminal input must supply an app name or ID.

For a shell inside the Controller container, retain terminal input:

```bash
docker compose exec controller phelix status
docker compose exec controller phelix remove
```

For Docker applications, `list` and the top `status` row use the serving
deployment's live status, recorded host PID and start time. The status row reads
memory and CPU from Docker. A partially running replica set appears as
`degraded`; a stopped deployment has PID zero and no uptime. Unknown start times
appear as `N/A`. The menu for `status --json` goes to stderr so stdout remains
JSON.

Use the same state path and service account for upgrades. Do not run a second
Controller against that directory. The persistent `controller.lock` uses an OS
file lock, which releases after the Controller and any surviving adapters exit.
Never delete that lock file while a Controller is running. Local filesystems with
reliable file locking are required; shared network filesystems are unsupported.

## Controller configuration

This file configures the Controller, independently of application `phelix.yaml`
and the embedded backend connection configuration.

```yaml
mode: container
network: production-backend
socket: /run/phelix/control.sock
socket_uid: 1000
max_operations: 8
sources:
  - host: /home/deployer/projects
    controller: /workspace
```

`mode` defaults to `host`; `container` requires a shared Docker network.
`max_operations` bounds simultaneous command adapters, from 1 to 128.
`socket_uid` grants one host UID ownership of the shared socket and its dedicated
parent directory. The default parent mode is 0700 and socket mode is 0600.
Root and the Controller UID remain authorized by Linux peer credentials.

For several operators, set `socket_gid` to a dedicated host group and list their
numeric UIDs in `allowed_uids`. The parent becomes 0750 and socket 0660. Group
membership grants filesystem access; peer authentication still checks the UID.
Container and host numeric IDs must match. A root Controller can set ownership;
a non-root Controller needs permission to set the configured UID and GID.
Use a dedicated socket directory because startup applies these permissions.

Controller management adapters and peer authentication require Linux. The
existing experimental Windows runtime does not have a supported Controller API.

## Builds and deployment

The request carries the CLI working directory. Host Mode accepts local paths;
configured `sources` restrict them. Container Mode maps host paths to explicit
Controller mounts. Symlinks escaping a configured root are refused. Supply
Dockerfile and .dockerignore in read-only mounts so builds need not generate them.
Missing sources return an actionable error before a build starts. Requests made
inside the Controller, including backend-triggered rebuilds, may use the mounted
Controller path and pass the same root and symlink checks.

```yaml
name: myapp
port: 8080
deploy:
  runtime: docker
  strategy: blue-green
  network: production-backend
health:
  endpoints:
    - name: readiness
      path: /health
      mode: auto
```

Run from the project directory:

```bash
phelix build myapp
phelix deploy myapp --strategy=rolling --replicas=2
phelix log myapp --no-follow --lines=50
phelix rollback myapp --to=v1 --reason=incident --verify=10s
phelix stop myapp
phelix start myapp
phelix restart myapp
phelix remove myapp --purge
```

Native classic deployment remains available in Host Mode. Docker requires
blue-green, rolling, canary or progressive. Docker builds use the Docker CLI and
the application's build stage; no application compiler is required in the
Controller image. Native builds require the corresponding host toolchain.
Keep secrets in the encrypted store and reference their names in project
configuration. API results and progress redact secret values.

## Operations and other interfaces

The separate `phelix.control.v1.Control` protobuf service defines Health, Info,
Query, Execute, GetOperation, WatchOperation and CancelOperation. Query returns
explicit application, runtime, version, history and metric messages. Execute
accepts the validated management command tree, source directory and bounded input;
it acknowledges a queued operation rather than claiming deployment success.
WatchOperation supplies ordered output and a terminal result. The CLI waits for
that result and preserves exit codes and JSON envelopes.

Operation IDs and results are durable in `controller-operations`. Progress is
bounded to the latest 512 events of at most 8192 bytes each and is retained in
memory until restart. A client can replay by sequence number. CLI stream failures
include the operation ID for later inspection:

```bash
phelix controller operation OPERATION_UUID
phelix controller operation OPERATION_UUID --cancel
```

Build, rebuild and log operations support cancellation. Unsafe operations reject
explicit cancellation and settle before ownership is released on shutdown.
The operation timeout defaults to one hour; API clients can request 1 to 86400
seconds. On Controller shutdown adapters receive an interrupt and cleanup is
allowed to finish. Interrupted durable records are recovered on the next start.
Keep the service shutdown grace long enough for the configured drain period.

Build/deploy/rollback, application lifecycle, Docker image operations, workspace
and matrix operations, plans, secrets, health, proxy configuration, logs,
security, execution authorization and session commands route through the API.
MCP stdio remains local transport and submits its tools through the same API,
preserving the separate MCP authorization actor. Backend-triggered subprocess
commands also use the local API.

Intentional local exceptions are help/completion/version, init (project setup),
doctor (local host diagnostics), update (CLI binary maintenance), and auth
(backend credential exchange). The wizard menu stays local and delegates its
selected management actions to the API. Controller startup and queries manage
or connect to the service. These exceptions execute no deployment logic in the
host CLI. Management adapters have no terminal; supply application identifiers
for start, stop, restart, rollback and other application actions. Authenticate
backend monitoring inside the Controller's state namespace in Container Mode.

## Networking and persistence

Host Mode Docker instances publish 8080 on an ephemeral host loopback port.
Container Mode publishes no application ports: the Controller and application
containers share one user-defined Docker network. Health and proxy targets use
each exact container IP and port 8080. No shared application alias is created.
Compose publishes the Controller proxy's public application ports explicitly.
The same deployment engine performs health-gated traffic changes and drains.

State adds endpoint fields without replacing the existing deployment format.
Restart inspects owned containers, refreshes endpoints, recovers journals and
restores proxy routes. AgentID, ServerID and application IDs stay in the original
registry. Containers carry managed, app, app-id, slot and Controller labels;
names alone never authorize cleanup. Other installations are protected.

Persist the complete data directory, including identity, apps.json, deployment
and version state, environment snapshots, master.key, secret metadata, journals,
logs, sessions and controller.yaml. Moving state requires stopping the old
Controller and copying the complete directory. The key/environment/session
migration from HOME/.phelix is repeatable, keeps originals and rejects conflicts.
Do not merge different installations or regenerate a missing encryption key.

The Controller collects Docker stats independently of deployment requests.
Metrics include CPU, memory and configured memory limit, network and block I/O,
state, restart count, health and exit code. Native backend metrics retain their
process sampler. Missing or stale Docker metrics return Unavailable to API
clients rather than zero measurements. The first sample can take several seconds
after deployment; retry after the next five-second sampling tick. Both logging
paths use redacted drains.

## Container security and troubleshooting

Docker socket access is effectively administrative access to the host Docker
environment. A read-only socket mount does not make Docker operations read-only.
Never expose the Engine through unauthenticated TCP. Restrict the Controller's
source, state and socket mounts; do not mount the entire host filesystem.
Compose owns Redis, PostgreSQL and other infrastructure; Phelix owns application
containers. Never run `compose up` for an application Phelix is deploying.

If CLI connection fails, check service logs, the socket override, numeric UID,
parent directory permissions and Controller health. For build failures, verify
the source mount exists inside the Controller. For traffic failures, verify the
Controller and application network, and publish the proxy port in Compose.
Retain state and journals when recovering from interrupted deployment. Inspect
status before retrying an interrupted operation.

Complete examples: [Host Mode](../../examples/docker-host-controller/README.md)
and [Container Mode](../../examples/docker-container-controller/README.md).
