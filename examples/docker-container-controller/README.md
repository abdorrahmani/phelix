# Container Mode with a shared Docker Controller

The host CLI is optional. Compose runs one Controller, Redis and
PostgreSQL. Phelix builds and owns the sample application containers on the same
Docker host. Application images contain only the Go application.

## Setup and startup

Use a Linux Docker host with Compose. Install the host CLI using the one-line
installer:

```bash
curl -fsSL https://phelix.anophel.com/install.sh | bash -s -- --no-service
```

If the installer started `phelix.service`, stop that host service before starting
the Controller container. This example runs its Controller inside Docker:

```bash
sudo systemctl stop phelix
```

Use the standalone `docker-container-controller.tar.gz` release asset, or open
this example directory in a source checkout. From a source checkout:

```bash
cd examples/docker-container-controller
PHELIX_VERSION="$(curl -fsSL https://phelix.anophel.com/releases/latest/version)"
./setup.sh "$PHELIX_VERSION"
docker compose pull phelix
docker compose up -d --wait
export PHELIX_CONTROL_SOCKET="$PWD/run/control.sock"
phelix phelix health
phelix phelix info
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
phelix phelix runtime container-example
phelix phelix metrics container-example
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
phelix phelix versions container-example
phelix rollback container-example --to=v1 --reason=example --verify=5s
phelix phelix history container-example
```

Rolling can use `--strategy=rolling --replicas=2`. Canary and progressive use
the existing `deploy.rollout` project configuration. All strategies use the same
deployment engine as Host Mode. The sample handles SIGTERM for graceful drain.

## Recreation and upgrades

Record `phelix phelix info`, then recreate only the Controller:

```bash
cd ..
docker compose up -d --force-recreate phelix
phelix phelix health
phelix phelix info
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
pull the chosen published Controller image, and recreate the Controller
with the same volumes. Update the host CLI to the same version. Never run old and
new Controllers together against one state volume. Do not remove the state volume
or regenerate identity to upgrade. Backend login, if needed, runs with
`docker compose exec phelix phelix auth login` so credentials persist there.

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

Use matching CLI and Controller versions. A release archive includes `VERSION`,
so `./setup.sh` selects that release without a repository checkout. `.env` retains
its selected image on repeated setup; change `PHELIX_IMAGE` there deliberately
for an upgrade. The main Compose file pulls the official image.

To use the CLI inside the Controller:

```bash
docker compose exec -T -w /workspace/app phelix phelix build container-example
docker compose exec -T phelix phelix deploy container-example
docker compose exec phelix phelix status
docker compose exec phelix phelix remove
```

For local development from a Phelix checkout, use `compose.build.yaml` as an
additional Compose file. The [installation guide](../../docs/guides/container-installation.md)
covers system installation, backup, upgrades and migration.
