# Host Mode with Docker applications

Phelix runs directly on a Linux host. Compose owns Redis and PostgreSQL; the
Controller builds and owns the sample application's containers. The sample
provides `/health`, checks configured infrastructure TCP endpoints, logs startup,
and drains on SIGTERM. Application images contain only the Go application.

## Setup

Install Docker Engine with Compose and install Phelix using the one-line
installer. The Controller's service account needs Docker socket access:

```bash
curl -fsSL https://phelix.anophel.com/install.sh | bash
```

If the installer started `phelix.service`, stop it before running this example's
foreground Controller with its separate state directory:

```bash
sudo systemctl stop phelix
```

Use the standalone `docker-host-controller.tar.gz` release asset, or open this
example in a source checkout. From a source checkout:

```bash
cd examples/docker-host-controller
./setup.sh
docker compose up -d --wait
export PHELIX_DATA_DIR="$PWD/state"
export PHELIX_CONTROLLER_CONFIG="$PWD/controller.yaml"
export PHELIX_CONTROL_SOCKET="$PWD/state/control.sock"
phelix controller
```

`setup.sh` creates a random database password in a mode 0600 `.env`, a persistent
state directory and Controller source configuration using Bash and standard
Linux utilities. `.env.example` lists the
required infrastructure variable. Keep state and `.env` out of version control.
Redis and PostgreSQL have persistent volumes and no published host ports.

## Deploy from another terminal

Set the same three PHELIX variables using absolute paths to this example.
The Controller must remain running. Build and configure the application:

```bash
cd examples/docker-host-controller
export PHELIX_DATA_DIR="$PWD/state"
export PHELIX_CONTROLLER_CONFIG="$PWD/controller.yaml"
export PHELIX_CONTROL_SOCKET="$PWD/state/control.sock"
phelix controller health
cd app
phelix build host-example
phelix env set host-example REDIS_ADDRESS=redis:6379 DATABASE_ADDRESS=postgres:5432
phelix deploy host-example
curl --fail http://127.0.0.1:8080/health
curl --fail http://127.0.0.1:8080/
phelix status host-example
phelix logs host-example --no-follow --lines=20
phelix controller metrics host-example
```

If metrics report `Unavailable` immediately after deployment, retry after five
seconds; this is the background sampler waiting for its first measurement.


Health returns `ready`, and `/` returns `Phelix example v1`. The second build
captures the configured encrypted environment. The public proxy listens on
port 8080; enforce the host firewall policy for external access. Container ports
are published only on ephemeral loopback ports. App and infrastructure containers
share `phelix-host-example`, without ambiguous application DNS aliases.

## Upgrade, rollback and recovery

```bash
sed -i 's/var version = "v1"/var version = "v2"/' main.go
phelix deploy host-example --strategy=blue-green
curl --fail http://127.0.0.1:8080/
phelix controller versions host-example
phelix rollback host-example --to=v1 --reason=example --verify=5s
phelix controller history host-example
phelix stop host-example
phelix start host-example
phelix restart host-example
```

The deployment switches only after readiness succeeds. Rollback restores the
recorded v1 image; environment compatibility follows the existing secret and
snapshot policy. Restart the Controller with Ctrl+C and `phelix controller` in
its terminal. Identity, registry and deployment state stay under `state`; owned
containers remain running and routes are recovered. For service installation,
set these same environment variables in the `phelix.service` systemd unit.

## Cleanup

Remove the application while the Controller is running, then stop the Controller:

```bash
phelix remove host-example --purge
cd ..
docker compose down
```

Compose down retains infrastructure volumes. Use `docker compose down --volumes`
only when deliberately discarding database data. Delete `state` only after
stopping the Controller and when deliberately discarding Phelix identity and
history. Application images are retained by the version retention policy.

Docker socket access grants administrative power over the Docker host. A
read-only socket mount would still permit mutations. See the
[Controller guide](../../docs/guides/controller.md) for permissions and upgrades.

Use a Phelix release that includes `phelix controller`; the installer downloads a
published release, not the code in your local checkout.
