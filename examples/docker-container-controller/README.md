# Container Mode with a shared Docker Controller

The host runs only the Phelix CLI. Compose runs one Controller, Redis and
PostgreSQL. Phelix builds and owns the sample application containers on the same
Docker host. Application images contain only the Go application.

## Setup and startup

Use a Linux Docker host with Compose. Install the host CLI using the one-line
installer:

```bash
curl -fsSL https://phelix.anophel.com/install.sh | bash
```

If the installer started `phelix.service`, stop that host service before starting
the Controller container. This example runs its Controller inside Docker:

```bash
sudo systemctl stop phelix
```

From the repository root:

```bash
cd examples/docker-container-controller
./setup.sh
docker compose build controller
docker compose up -d --wait
export PHELIX_CONTROL_SOCKET="$PWD/run/control.sock"
phelix controller health
phelix controller info
```

The health response contains `ready: true`. `setup.sh` writes Controller
configuration with your numeric host UID, an absolute source mapping, and a
random database password in mode 0600 `.env` using Bash and standard Linux
utilities. `controller.example.yaml` and
`.env.example` describe those inputs. Use a dedicated `run` directory for the
socket; Controller startup adjusts its ownership and permissions.

The Controller runs as root inside the container to access the mounted local
Docker socket and assign socket ownership. Its local API remains protected by
0600 socket permissions, a 0700 parent directory and Linux peer UID checks.
For another operator UID/GID, follow the shared-group configuration in the
[Controller guide](../../docs/guides/controller.md).

## Deploy and inspect

```bash
cd app
phelix build container-example
phelix env set container-example REDIS_ADDRESS=redis:6379 DATABASE_ADDRESS=postgres:5432
phelix deploy container-example
curl --fail http://127.0.0.1:8080/health
curl --fail http://127.0.0.1:8080/
phelix status container-example
phelix logs container-example --no-follow --lines=20
phelix controller runtime container-example
phelix controller metrics container-example
```

If metrics report `Unavailable` immediately after deployment, retry after five
seconds; this is the background sampler waiting for its first measurement.


Health returns `ready`; `/` returns `Phelix example v1`. The application's
Dockerfile builds Go inside a build stage, so the Controller needs no Go compiler.
The source bind mount is read-only at `/workspace/app`, and the host working path
maps to it through `sources`. Both Dockerfile and .dockerignore are supplied.
No host source path is assumed to exist inside the Controller.

The Controller and application instances share `phelix-container-example`.
Health checks and proxy targets use exact container IPs at port 8080. Application
containers publish no ports and have no shared app alias. Compose publishes
only the Controller proxy at host loopback port 8080. Change that binding and
your firewall deliberately when exposing application traffic externally.

## Deploy another version and roll back

```bash
sed -i 's/var version = "v1"/var version = "v2"/' main.go
phelix deploy container-example --strategy=blue-green
curl --fail http://127.0.0.1:8080/
phelix controller versions container-example
phelix rollback container-example --to=v1 --reason=example --verify=5s
phelix controller history container-example
```

Rolling can use `--strategy=rolling --replicas=2`. Canary and progressive use
the existing `deploy.rollout` project configuration. All strategies use the same
deployment engine as Host Mode. The sample handles SIGTERM for graceful drain.

## Recreation and upgrades

Record `phelix controller info`, then recreate only the Controller:

```bash
cd ..
docker compose up -d --force-recreate controller
phelix controller health
phelix controller info
curl --fail http://127.0.0.1:8080/health
```

AgentID and ServerID must match the previous response. Application IDs, retained
images, encrypted snapshots, keys, journals and logs use the persistent `state`
volume. The socket uses the bind-mounted `run` directory. Configuration and source
use explicit bind mounts. Startup checks these mounts and the shared network.
Application containers survive Controller recreation; startup refreshes endpoints
and restores proxy routes. A Controller outage interrupts proxy traffic until
recovery, even though application containers remain running.

For an upgrade, back up the entire state volume with the Controller stopped,
build the new Controller image from reviewed code, and recreate the Controller
with the same volumes. Update the host CLI to the same version. Never run old and
new Controllers together against one state volume. Do not remove the state volume
or regenerate identity to upgrade. Backend login, if needed, runs with
`docker compose exec controller phelix auth login` so credentials persist there.

## Cleanup and security

```bash
cd app
phelix remove container-example --purge
cd ..
docker compose down
```

Compose down retains all named volumes. Use `--volumes` only when intentionally
discarding database data and Controller identity. Stop/remove Phelix-managed
applications through Phelix before removing their shared network.

Mounting the Docker socket is effectively administrative access to the host
Docker environment. A read-only socket mount does not restrict Docker API
operations. Never expose the socket over unauthenticated TCP. Limit who can use
the Control API and modify Controller configuration or source. Compose owns
infrastructure services; it must never simultaneously own application containers
Phelix deploys. The only Phelix installation and identity inside Docker is the
Controller.

Use matching CLI and Controller versions with Controller support. The installer
downloads a published release, while Compose builds the Controller from this
checkout; unpublished local changes are not included in the installed CLI.
