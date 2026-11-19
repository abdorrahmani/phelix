# Monitoring & Multi-Server

Phelix can push app data, resource metrics, health, and logs to your dashboard
at `phelix.anophel.com` through a persistent gRPC monitoring stream. Monitoring
is **opt-in per app** and requires an authenticated session.

## Controller and monitoring

`phelix controller` starts the service that owns local application management,
deployment, recovery, the proxy and monitoring. `phelix monitor` is a compatibility
alias for the same service. Do not run both as separate processes.

Monitoring is a responsibility within the Controller. Its optional backend
connection is separate from the local Unix socket used by the CLI. Local
application management does not require dashboard authentication or a watched app.
The Controller:

- Restores managed applications that were previously running (auto-start apps).
- Opens a single, long-lived, TLS-secured gRPC stream to the Phelix backend
  (requires an authenticated session — see [Authentication](authentication.md)).
- Monitors application status across all servers.
- Sends application information, resource metrics, and logs of every **watched**
  app to the central server roughly every 2 seconds (see per-app watching below).
- While at least one app is watched, also sends the server's own data — identity,
  host metrics, self logs and metadata. An authenticated agent connection can
  remain active without watched apps, advertising capabilities and heartbeats.
- Automatically reconnects with exponential backoff if the connection is lost.
- Provides real-time updates for all managed applications.

It runs in the **foreground** and stays attached to the terminal when invoked
manually. On Linux it is normally started by the systemd unit created by
`install.sh`, which supervises `phelix controller` directly
(`/usr/local/bin/phelix controller` as `ExecStart`). There is no backgrounding
shell wrapper. Manage it with:

```bash
sudo systemctl start phelix
sudo systemctl restart phelix
sudo systemctl stop phelix
sudo systemctl status phelix
sudo journalctl -u phelix -f
```

## Enable backend reporting

With the installed Controller service running, authenticate as its service
account and enable watching for each application you want on the dashboard:

```bash
phelix auth login --scope agent
phelix watch myapp
phelix status myapp
```

Use the same `PHELIX_DATA_DIR` as the Controller if you configured a custom state
directory. Login alone does not enable watching. A full-scope login also works;
the Controller prefers an agent-scoped session when available.

In Container Mode, login inside the Controller container so the session is
stored in its persistent state volume. For the supplied Compose example:

```bash
docker compose exec controller phelix auth login --scope agent
phelix watch container-example
```

Run the host `watch` command with the example's `PHELIX_CONTROL_SOCKET` configured.
The Controller sends watched app data through its existing authenticated gRPC
connection. A separate `phelix monitor` process is unnecessary.

## Multi-server

Phelix can monitor multiple servers simultaneously. Each server running Phelix
will:

- Register itself with the central monitoring service
- Send regular status updates
- Maintain its own application state
- Sync with other servers when needed

## Per-app watching (`watching`)

Each application has a `watching` state that decides whether **that app**
participates in backend monitoring/reporting. It is a per-app opt-in with
**`disabled` as the default** — new apps, and apps from installations that
predate the feature, are never monitored remotely until you say so.

```bash
phelix watch api              # enable:  api's monitoring data goes to the backend
phelix watch api --disable    # disable: no monitoring data for api
```

You can also declare the state in `phelix.yaml`:

```yaml
watching: enable   # or disable; `phelix init` writes disable
```

When the key is present, `phelix build` and `phelix rebuild` apply its value to
the app — the project file is the desired state, exactly like the `health:`
block. A `phelix.yaml` without the key (older projects) leaves the app's current
state untouched. `phelix watch` toggles the runtime state and also updates the
`watching:` key in the project's `phelix.yaml`, so the two can never diverge: a
rebuild keeps your choice instead of reverting it (a project without
`phelix.yaml` changes only the runtime state — no file is created).

The state persists in `apps.json`, is shown by `phelix list` (WATCHING column)
and `phelix status` (Watching column), and takes effect on the Controller's
next reporting tick — no restart needed.

**When `watching` is enabled**, the app participates in backend monitoring as
usual: app metrics, request traffic (request count, error rate, p95 latency,
per-replica share), health snapshots, app logs, deployment topology, and
dashboard registration flow to `phelix.anophel.com`.

**When `watching` is disabled**, none of that data is sent for the app — the
backend receives no telemetry payloads for that app.
Everything else keeps working exactly as before: the app can still be built, run,
stopped, restarted, rolled back, and inspected locally, local health checks and
auto-restart keep running, and `phelix list`/`phelix status` show its full local
state. `watching` belongs to the application, so one server can mix watched and
unwatched apps freely.

**Host telemetry follows the same opt-in.** Detailed server information, host
metrics, self logs and metadata sync are transmitted only while at least one app
is watched. The authenticated agent connection can remain open without watched
apps: it sends the server ID, agent version, capabilities and heartbeats, and can
receive remote commands subject to local authorization and reporting consent.
Enabling watching takes effect without restarting the Controller or its agent
connection.

Watching never affects local behavior: `phelix build` and `phelix rebuild` work
identically with or without it (a missing `phelix.yaml` changes nothing), and a
project directory without the `watching:` key keeps the app's current state.

This is primarily useful for **Free-plan users** who want to choose which
application/server combination is monitored remotely instead of sending
monitoring data for every local application. The CLI itself remains free and
local: there are no subscription checks on this flag, and backend plan quotas are
enforced server-side.

## Remote (dashboard-issued) commands

While the Controller has an authenticated backend connection, the dashboard can
issue remote commands over the monitoring stream — remote rebuild (including one-off strategy
overrides), rollback, matrix, and webhook management. The one-off strategy
override the dashboard sends is the same one `--strategy` applies locally; see
[configuration](../reference/configuration.md) and the

## Related

- [Controller operations](controller.md): installation, local API and lifecycle.
- [Authentication](authentication.md) — the session the daemon needs, and
  agent-scoped tokens.
- [Health checks](health-checks.md) — how health snapshots are configured.
- [Monitoring architecture](../architecture/monitoring.md) — the gRPC stream,
  health-daemon reconcile loop, and snapshot semantics.
- [Logging pipeline](../architecture/logging.md): self logs, wire fields, and
  backend changes for log batches.
