# Installing the Docker Controller

Host Mode runs the Controller through systemd and supports native and Docker
applications. Container Mode runs the same Controller and local management API
inside Docker and supports Docker applications. The host CLI is optional.
Docker Engine builds and runs applications; application Dockerfiles provide
compilers. Installing Go or Rust on the host or in the Controller is unnecessary
for Docker builds.

## Fresh installation

Use a Linux amd64 or arm64 host with Docker Engine and Docker Compose already
installed. The invoking account must have Docker socket access and permission
to create system directories, directly or through sudo. The installer uses Bash.
A version becomes available only after its release workflow publishes the image.

```bash
curl -fsSL https://phelix.anophel.com/install.sh | bash -s -- --docker --with-cli --proxy-port 8080
```

Omit `--with-cli` to operate entirely through container commands. Use `--version`
to pin a published release or `--channel beta` to choose the beta channel. The
installer resolves a version before pulling `ghcr.io/abdorrahmani/phelix:<version>`.
For versions containing `+` metadata, the image tag uses `_` in its place.

Defaults and overrides:

| Location | Default | Installer option |
| --- | --- | --- |
| Configuration and Compose | `/etc/phelix` | `--config-dir` |
| Complete persistent state | `/var/lib/phelix` | `--data-dir` |
| Shared API directory | `/var/run/phelix` | `--socket-dir` |
| Host projects | `/srv/phelix/projects` | `--projects-dir` |
| Shared Docker network | `phelix` | `--network` |
| Published proxy ports | None | Repeat `--proxy-port` |

Proxy ports are published on host loopback. Edit Compose bindings and your
firewall deliberately to serve external traffic. The management API has no
public port. Compose creates the configured network; Phelix owns the application
containers. Additional proxy ports require recreating the Controller.

Installation refuses an active or enabled host service, existing state, an existing
Container Mode configuration, or an existing host CLI endpoint file. It does
not install Docker or replace an existing installation. A repeated installation
leaves the original configuration and state intact. Use the upgrade or migration
instructions below instead.

The optional host CLI uses `/etc/phelix/client.yaml` automatically. Explicit
`PHELIX_CONTROL_SOCKET`, `PHELIX_CONTROLLER_CONFIG`, or `PHELIX_DATA_DIR` settings,
and a user's existing Controller configuration take precedence. Remove stale
shell overrides when switching modes. Authentication for backend monitoring must
run inside the Controller so credentials are stored in its persistent state:

```bash
docker compose -f /etc/phelix/compose.yaml exec phelix phelix auth login --scope agent
```

## Build and deploy

Prepare a project under the configured host root with `phelix.yaml`, Dockerfile
and `.dockerignore`. Set `deploy.runtime: docker` and use the configured shared
network. The runnable examples include these files. Preserve your existing
Dockerfile, including its language builder stage and build settings.

The root is mounted read-only at `/projects`. For example,
`/srv/phelix/projects/my-api` maps to `/projects/my-api`. Escaping symlinks,
traversal, missing mounts and overlapping roots are rejected. To generate or edit
project files through Phelix, temporarily allow a writable source mount and
restore read-only access afterwards. Build and deployment use Docker's existing
build cache. Supporting infrastructure may use Compose on the same network;
Compose must not also manage Phelix application containers.

With the host CLI:

```bash
cd /srv/phelix/projects/my-api
phelix controller health
phelix build my-api
phelix deploy my-api
phelix status my-api
phelix logs my-api --no-follow --lines=20
phelix rollback my-api --to=v1 --reason=manual --verify=5s
```

Without a host CLI:

```bash
docker compose -f /etc/phelix/compose.yaml exec -T -w /projects/my-api phelix phelix build my-api
docker compose -f /etc/phelix/compose.yaml exec -T phelix phelix deploy my-api
docker compose -f /etc/phelix/compose.yaml exec phelix phelix status
```

`status` and `remove` retain interactive application selection. Specify an app
when running without a terminal. Docker status and metrics come from the Engine;
retry metrics after its first sample if they are temporarily unavailable.

## Restart, backup and upgrade

Controller restart briefly interrupts its in-process proxy. Application
containers remain alive; startup restores their endpoints and routes. Controller
upgrades are not zero downtime. Do not run two Controllers against the same
state, and do not delete the state directory to upgrade.

```bash
docker compose -f /etc/phelix/compose.yaml restart phelix
docker compose -f /etc/phelix/compose.yaml exec -T phelix phelix controller health
docker compose -f /etc/phelix/compose.yaml exec -T phelix phelix controller info
```

Before upgrading, settle active operations, record Controller information,
application status and proxy health, then stop the Controller. Adapt paths if
you changed defaults. Keep backups outside the state directory with restricted
permissions. Backup includes identity, registry, versions, history, encryption
keys, environment snapshots, secrets, sessions, logs and operation journals.

```bash
docker compose -f /etc/phelix/compose.yaml stop phelix
sudo sh -c 'umask 077; tar -C /var/lib -czf /root/phelix-state-backup.tar.gz phelix'
sudo cp -p /etc/phelix/compose.yaml /etc/phelix/compose.yaml.before-upgrade
sudo cp -p /etc/phelix/controller.yaml /etc/phelix/controller.yaml.before-upgrade
```

Change only the Compose image to the chosen published version, then run:

```bash
docker compose -f /etc/phelix/compose.yaml pull phelix
docker compose -f /etc/phelix/compose.yaml up -d --no-deps --wait --wait-timeout 90 phelix
docker compose -f /etc/phelix/compose.yaml exec -T phelix phelix version --short
docker compose -f /etc/phelix/compose.yaml exec -T phelix phelix controller info
docker compose -f /etc/phelix/compose.yaml exec -T phelix phelix list
```

Verify the identity matches, the registry is unchanged, proxy requests succeed,
and logs and rollback remain available. Keep the old image and backup until this
passes. Update an optional host CLI to the same version using `--no-service`.
Container images are upgraded with Compose. Binary self-update inside the image
is rejected. The backend's systemd restart action is unavailable in Container
Mode; restart the Controller with Compose on its host. Other existing backend
monitoring and application management paths remain in place.

For rollback, stop the new Controller, retain its current state separately,
restore the compatible pre-upgrade state backup, and restore the previous image
and configuration. Do not silently run an older image against a changed state
schema. No state format migration is introduced by this change.

## Migrating an existing host installation

Migration is deliberately a reviewed backup-and-restore operation. Automatic
adoption of an existing store would risk secrets, absolute paths and running
applications. The installer refuses to perform it.

1. Record `phelix controller info`, `phelix list`, each app's runtime, current
   version, source directory, network and proxy port. Inspect active operation
   records and finish or cancel safe work. Stop native apps through Phelix; retain
   Host Mode if they must continue running. Docker workers and jobs remain
   unsupported, as in the existing runtime.
2. Stop and disable `phelix.service` to prevent ownership returning after reboot.
   Keep its unit and executable for recovery. Back up the complete existing state
   directory, usually the service user's `.phelix`, plus source configuration and
   any legacy key/session/environment store. Do not remove app containers.
3. Copy the complete state into a separate persistent target while preserving
   permissions. Preserve every saved absolute state and project path inside the
   Controller with matching bind targets. Do not rename stored application paths
   or snapshot paths blindly. The simplest migration mounts the existing state
   and projects at their original absolute paths.
4. Prepare a Controller configuration with Container Mode, the existing shared
   application network, a shared API socket, the authorized operator UID, and
   explicit source mappings. Mount state writable, source/configuration
   read-only, and the local Docker socket. Attach the Controller to that existing
   network as an external Compose network. Preserve application IDs and ownership
   labels. If applications span different networks, first prepare one supported
   shared network and review their project network settings.
5. Pull the matching official image and start the Controller using the reviewed
   Compose configuration. Never run the old service concurrently. Verify the
   original Agent ID and Server ID, secrets, retained versions, app status,
   recovered routes and actual proxy traffic before accepting migration.
6. Configure the host CLI with the shared socket in `/etc/phelix/client.yaml`, or
   operate inside the Controller. Keep the original store and backup. To revert,
   stop the container Controller, restore the original compatible store and
   service configuration, then re-enable and start the host service.

Use the container example's Compose file as a starting point, replacing its
sample mounts and network with the reviewed migration paths. Do not use
`docker compose down --volumes` as part of migration or upgrades.

## Permissions and troubleshooting

Docker socket access grants administrative power over the host. A read-only
socket mount still allows Docker operations. The default image runs as root to
access the socket and assign shared socket ownership. Non-root operation requires
matching numeric IDs and pre-created writable state/socket directories, plus
Docker socket group access. Never expose Engine TCP for this installation.

State is private, the configuration is mode 0600, and the client endpoint file
contains only a socket path. The socket uses restrictive directory/file modes
and Linux peer UID authorization. Group permission alone does not authorize API
calls. Keep untrusted users away from the socket, source and configuration.

If readiness fails, use `docker compose -f /etc/phelix/compose.yaml logs phelix`.
Check Docker socket access, network membership, writable persistent mounts and
native app recovery restrictions. Missing projects need a configured source
mount; missing public ports need a Compose proxy binding. An unavailable API
needs a running Controller and the correct socket. Never regenerate identity or
delete state to resolve a startup problem.
