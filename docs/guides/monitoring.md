# Monitoring & Multi-Server

Phelix can push app data, resource metrics, health, and logs to your dashboard
at `phelix.anophel.com` through a persistent gRPC monitoring stream. Monitoring
is **opt-in per app** and requires an authenticated session.

## `phelix monitor`

Starts the long-running gRPC monitoring daemon that:

- Restores managed applications that were previously running (auto-start apps).
- Opens a single, long-lived, TLS-secured gRPC stream to the Phelix backend
  (requires an authenticated session — see [Authentication](authentication.md)).
- Monitors application status across all servers.
- Sends application information, resource metrics, and logs of every **watched**
  app to the central server roughly every 2 seconds (see per-app watching below).
- While at least one app is watched, also sends the server's own data — identity,
  host metrics, self logs, agent metadata; with nothing watched it transmits
  nothing at all and does not even open the stream.
- Automatically reconnects with exponential backoff if the connection is lost.
- Provides real-time updates for all managed applications.

It runs in the **foreground** and stays attached to the terminal when invoked
manually. On Linux it is normally started by the systemd unit created by
`install.sh`, which supervises `phelix monitor` directly
(`/usr/local/bin/phelix monitor` as `ExecStart`) — there is no backgrounding
shell wrapper. Manage it with:

```bash
sudo systemctl start phelix
sudo systemctl restart phelix
sudo systemctl stop phelix
sudo systemctl status phelix
sudo journalctl -u phelix -f
```

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
and `phelix status` (Watching column), and takes effect on the monitor daemon's
next reporting tick — no restart needed.

**When `watching` is enabled**, the app participates in backend monitoring as
usual: app metrics, health snapshots, app logs, deployment topology, and
dashboard registration flow to `phelix.anophel.com`.

**When `watching` is disabled**, none of that data is sent for the app — the
backend receives no monitoring stream for it at all (not an empty payload).
Everything else keeps working exactly as before: the app can still be built, run,
stopped, restarted, rolled back, and inspected locally, local health checks and
auto-restart keep running, and `phelix list`/`phelix status` show its full local
state. `watching` belongs to the application, so one server can mix watched and
unwatched apps freely.

**Server-level data follows the same opt-in.** Server identity, host metrics,
self logs, and agent metadata are transmitted only while at least one app on the
server is watched: a server whose every app has opted out sends nothing to the
backend at all, and the monitor daemon does not even open the monitoring stream
(there is nothing to say, and a stream that never identifies itself would be
reaped by the backend's pre-bind window anyway). Enabling `watching` on any app
re-opens the stream within a few seconds — no restart needed. This also means
dashboard-issued remote commands (rollback, matrix, webhook management) cannot
reach a server while none of its apps are watched; enabling watching restores the
channel.

Watching never affects local behavior: `phelix build` and `phelix rebuild` work
identically with or without it (a missing `phelix.yaml` changes nothing), and a
project directory without the `watching:` key keeps the app's current state.

This is primarily useful for **Free-plan users** who want to choose which
application/server combination is monitored remotely instead of sending
monitoring data for every local application. The CLI itself remains free and
local: there are no subscription checks on this flag, and backend plan quotas are
enforced server-side.

## Remote (dashboard-issued) commands

While a server has at least one watched app, the dashboard can issue remote
commands over the monitoring stream — remote rebuild (including one-off strategy
overrides), rollback, matrix, and webhook management. The one-off strategy
override the dashboard sends is the same one `--strategy` applies locally; see
[configuration](../reference/configuration.md) and the
[remote webhook contract](../architecture/backend-contracts/remote-webhook-backend-contract.md).

## Related

- [Authentication](authentication.md) — the session the daemon needs, and
  agent-scoped tokens.
- [Health checks](health-checks.md) — how health snapshots are configured.
- [Monitoring architecture](../architecture/monitoring.md) — the gRPC stream,
  health-daemon reconcile loop, and snapshot semantics.
- Backend contracts:
  [agent capabilities](../architecture/backend-contracts/agent-capabilities-backend-contract.md),
  [docker-runtime telemetry](../architecture/backend-contracts/docker-runtime-telemetry-backend-contract.md).
- [Command reference](../reference/commands.md#lifecycle).
