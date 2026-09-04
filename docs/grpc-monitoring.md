# Phelix gRPC Monitoring — Backend Implementation Guide

**Audience:** backend engineers implementing/maintaining the Phelix backend
gRPC server. This document assumes no familiarity with the Phelix CLI
codebase.

**Status:** the WebSocket monitoring transport has been **removed**. gRPC is
now the only monitoring transport. There is no fallback to WebSocket, and no
plan to reintroduce it.

---

## 1. Architecture

```text
Phelix CLI Monitor Daemon (long-running foreground process, "phelix monitor")
          |
          | TLS + gRPC (persistent, bidirectional stream)
          |
          v
Phelix Backend  (PhelixService.MonitorStream)
          |
          v
Monitoring / Metrics / Database
```

- The CLI's `phelix monitor` command is a long-running foreground daemon
  (normally supervised directly by systemd). On startup it opens **one** gRPC
  connection to the backend and keeps it open indefinitely.
- Over that connection it opens **one** bidirectional stream,
  `PhelixService.MonitorStream`, and keeps *that* open indefinitely too.
- All monitoring data (server identity, server resource metrics, per-app
  resource metrics, per-app status/details, application logs, and the CLI's
  own daemon logs) flows over this single stream as a continuous sequence of
  `MonitorEvent` messages — roughly one batch every **2 seconds** per
  connected server.
- The backend may push `MonitorControl` messages back down the same stream
  at any time: remote commands (e.g. "restart this app") and keepalive
  pings.
- `MonitorStream` is one of several RPCs on `PhelixService`. The others
  (`ReportEvent`, `AgentStream`, `HealthSetConfig`, `ReportRollbackEvent`,
  etc.) are unrelated to this migration and already existed before it —
  see `internal/grpc/proto/phelix.proto` for the full service definition.

This replaces the previous architecture, which was:

```text
Phelix CLI Monitor Daemon --- WebSocket (JSON messages) ---> Phelix Backend
```

The `type`/`payload` JSON envelope used by the old WebSocket protocol is
now the protobuf `oneof` on `MonitorEvent`/`MonitorControl` described below.
Every field the old WebSocket payloads carried has a 1:1 equivalent here.

---

## 2. Proto Contract

Source of truth: `internal/grpc/proto/monitoring.proto` (imported by
`internal/grpc/proto/phelix.proto`, which declares the RPC). Generate the
backend's server stubs from these `.proto` files directly — do not
hand-transcribe the message shapes.

### 2.1 Service / RPC

```proto
service PhelixService {
  ...
  rpc MonitorStream(stream MonitorEvent) returns (stream MonitorControl);
}
```

- **Kind:** bidirectional streaming (see [§5](#5-streaming) for why).
- **Cardinality:** one call per connected CLI process. A single Phelix
  server host normally runs exactly one `phelix monitor` daemon, so expect
  one `MonitorStream` call per managed server, held open indefinitely.

### 2.2 `MonitorEvent` (CLI → backend)

```proto
message MonitorEvent {
  string server_id = 1;   // required — the reporting server's stable ID
  int64 timestamp = 2;    // required — event time, Unix ms (CLI clock)

  oneof payload {
    ServerInfo server_info = 10;
    ServerMetrics server_metrics = 11;
    AppResourceMetrics app_metrics = 12;
    ApplicationInfo app_info = 13;
    MonitorLogEntry log_entry = 14;
    MonitorCommandResult command_result = 15;
    Pong pong = 16;
  }
}
```

Exactly **one** oneof field is set per message — never more than one, and
never zero (a message with no payload should be ignored/logged as
malformed). Field numbers `10`+ are reserved for payload variants so new
signal types can be added later (`17`, `18`, …) without touching existing
consumers; do **not** renumber existing fields.

| Payload | Meaning | Frequency |
|---|---|---|
| `ServerInfo` | Static-ish server identity/specs (hostname, IPs, OS, CPU/mem/disk totals, uptime, region, arch, kernel). | Once per stream (re)connection. |
| `ServerMetrics` | Live host resource usage (CPU%, memory, disk, network, load average, swap, process count). | Every ~2s. |
| `AppResourceMetrics` | Per-app CPU% and memory usage. One message per managed app. | Every ~2s per app. |
| `ApplicationInfo` | Per-app identity/status (name, status, language, port, build status, PID, uptime, created/updated timestamps). One message per managed app. | Every ~2s per app. |
| `MonitorLogEntry` | One log line, either from the managed application or from the CLI's own daemon log (`source`). | Whenever new log lines exist (checked every ~2s tick, batched). |
| `MonitorCommandResult` | Result of executing a `MonitorCommandRequest` the backend previously sent (see §2.3). | In response to a command. |
| `Pong` | Keepalive reply to a backend-initiated `Ping`. | In response to a ping. |

#### `ServerInfo`

| Field | Type | Required | Notes |
|---|---|---|---|
| `id` | string | yes | Stable server ID, matches `server_id` on all other messages from this stream. |
| `hostname` | string | yes | |
| `ip_v4` / `ip_v6` | string | no | Comma-separated if multiple; may be empty. |
| `os_type`, `os_full` | string | yes | |
| `cpu_info` | string | no | CPU model string. |
| `network_ifaces` | repeated string | no | `"name(mac)"` entries. |
| `total_memory`, `total_storage`, `swap_total` | int64 | yes | Bytes. |
| `total_cpu_cores` | int32 | yes | |
| `uptime` | string | yes | Human-readable (`"1d 2h 3m"`), not machine-parseable — do not rely on format stability. |
| `last_reboot`, `created_at` | int64 | yes | Unix ms. |
| `status` | string | yes | One of `"healthy"`, `"warning"`, `"critical"` (thresholded on mem/disk %). |
| `region`, `architecture`, `kernel_version` | string | no | Best-effort. |
| `connection`, `alert`, `security` | message | no | Server-settings groups (fields `20`–`22`); see below. Absent/nil = the CLI has no configuration to report. |

#### `ServerMetrics`

All numeric fields required (zero is a valid value, not "unset"):
`server_id`, `used_memory`, `free_memory`, `memory_percent`,
`cpu_usage_percent`, `used_storage`, `free_storage`, `storage_percent`,
`timestamp` (Unix ms), `network_in`, `network_out` (cumulative byte
counters, not deltas — compute rate on the backend if needed),
`load_avg_1min`/`5min`/`15min`, `swap_used`, `swap_free`, `swap_percent`,
`running_processes`.

#### `AppResourceMetrics`

`app_id`, `server_id` (both required strings), `cpu_usage` (double, %),
`memory_usage` (uint64, bytes).

#### `ApplicationInfo`

`id`, `server_id`, `name`, `status`, `language`, `build_status` (all
strings, required except `language`/`build_status` which may be empty),
`port`, `pid` (int32; `pid` may be `0`/negative if the app isn't running),
`uptime` (string, human-readable), `created_at`, `updated_at` (int64, Unix
ms).

`status` is a free-form string produced by the CLI's app manager (e.g.
`"running"`, `"stopped"`, `"crashed"`) — treat it as an opaque enum-like
string, not a closed enum, since the CLI may add new statuses.

#### `ApplicationInfo` — configuration groups

`ApplicationInfo` additionally carries four optional configuration blocks:
`process`, `networking`, `logging`, and `storage` (fields `20`–`23`). Each is
a nested message populated whenever the CLI knows the corresponding values;
if a group is absent/nil the CLI has no configuration to report for it. The
backend should treat each group as optional and never require it. Field names
and semantics mirror the CLI's `AppProcessConfig` / `AppNetworkingConfig` /
`AppLoggingConfig` / `AppStorageConfig` types (see `internal/app/types.go`).
`repeated` string fields (`AppNetworking.allowed_origins`,
`AppStorage.volumes`) may be empty — an empty list means "none", not "unset".

- `AppProcess`: `working_dir`, `executable`, `start_command`, `stop_command`
  (strings); `max_cpu_percent`, `crash_loop_backoff` (seconds),
  `graceful_shutdown` (seconds), `max_restart_attempts`,
  `restart_delay_ms` (int32); `max_memory_mb`, `max_open_files`,
  `max_processes` (int64); `auto_restart` (bool). Zero/empty values mean the
  CLI has no explicit value (unlimited or OS default).
- `AppNetworking`: `listen_port` (int32), `bind_address`, `public_domain`,
  `base_path`, `cert_path`, `key_path` (strings), `tls_enabled`,
  `proxy_enabled`, `cors_enabled`, `rate_limit_enabled` (bool),
  `allowed_origins` (repeated string), `rate_limit_rps` (int32).
- `AppLogging`: `json_logs`, `persistent_logs`, `rotation_enabled`,
  `rotation_compress` (bool); `log_level`, `log_file_path`,
  `stderr_file_path`, `log_format` (strings); `rotation_max_size_mb`
  (int32, MiB); `rotation_max_files` (int32).
- `AppStorage`: `volumes` (repeated string), `data_dir`, `temp_dir`
  (strings).

#### `ServerInfo` — server-settings groups

`ServerInfo` additionally carries three optional settings blocks:
`connection`, `alert`, and `security` (fields `20`–`22`). Each is a nested
message that the CLI auto-detects from the current host state plus product
defaults, persists to `~/.phelix/settings.json` (0600), and reports once per
stream establishment alongside the identity snapshot. These are
**agent-reported DEFAULTS only** — the backend owns real configuration
through its own API. A nil/absent group means the CLI has no configuration
to report, exactly like `ApplicationInfo`'s config groups. See
`docs/server-settings-backend.md` for the full backend-facing contract
including direction (write-only/read-only) rules.

- `ServerConnection` (fields `1`–`6`): `ssh_port` (int32, default `22`),
  `ssh_user` (string, default `"phelix"`), `auth_method` (string,
  `"key"`/`"password"`, default `"key"`), `ssh_password` (string,
  **write-only**: sent agent→backend, never echoed back), `private_key`
  (string, **write-only**: sent agent→backend, never echoed back),
  `public_key` (string, **read-only**: agent→backend).
- `ServerAlert` (fields `1`–`9`): `cpu_threshold` / `ram_threshold` /
  `disk_threshold` (double, %; CLI defaults `80`/`90`/`90` — always populated
  with concrete values, treat `0` as a value not "unset"); `cpu_spike_alerts`,
  `memory_pressure_alerts`, `disk_space_alerts`, `app_crash_alerts`,
  `agent_disconnect_alerts`, `weekly_digest` (bool, default `false`).
- `ServerSecurity` (fields `1`–`6`): `firewall_enabled`, `auto_updates`,
  `ip_allowlist_enabled` (bool); `ssh_root_login` (string: sshd
  `PermitRootLogin` — `yes`/`no`/`prohibit-password`, default
  `"prohibit-password"`); `allowed_ips` (repeated string, empty = none);
  `open_ports` (repeated int32, distinct listening TCP ports, sorted).

#### `MonitorLogEntry`

`id`, `server_id`, `app_id` (may be empty for self logs), `log` (raw line,
required), `date` (int64 Unix ms), `level` (string: `INFO`/`WARNING`/`ERROR`/
`DEBUG`), `source` (`LogSource` enum: `LOG_SOURCE_APPLICATION = 1` or
`LOG_SOURCE_SELF = 2`; `LOG_SOURCE_UNSPECIFIED = 0` should never legitimately
appear and can be treated as `APPLICATION` for backward safety), `stream`
(`LogStream` enum: `LOG_STREAM_STDOUT = 1` / `LOG_STREAM_STDERR = 2`, set only
for application logs; `LOG_STREAM_UNSPECIFIED = 0` for self logs), `component`
(string, set only for self logs — the subsystem that produced the line, e.g.
`monitor`, `health`, `grpc`, `app`, `settings`, `logs`; empty for application
logs).

`stream` and `component` are mutually exclusive: `stream` identifies the app's
stdout/stderr stream, `component` identifies the agent subsystem. See
`docs/logging-backend.md` for the full log contract (on-disk line format,
level semantics, component registry, dedupe keys, retention guidance).

#### `MonitorCommandResult`

`request_id` (echoes the `MonitorCommandRequest.request_id` that triggered
it — use this to correlate, especially if you allow concurrent commands per
server in the future), `command`, `app_name`, `status` (`"success"` or
`"error"`), `error` (populated iff `status == "error"`), `timestamp` (Unix
ms).

### 2.3 `MonitorControl` (backend → CLI)

```proto
message MonitorControl {
  oneof payload {
    MonitorCommandRequest command = 1;
    Ping ping = 2;
  }
}

message MonitorCommandRequest {
  string request_id = 1;
  string type = 2;       // e.g. "restart", "stop", "start", "rebuild"
  string app_name = 3;    // app name OR app ID — CLI resolves either

  string strategy = 4;   // optional, "rebuild" only: classic|blue-green|rolling
  int32 replicas = 5;    // optional, rolling only; >= 1
}
```

- `command`: instructs the CLI to run a lifecycle action against a specific
  managed application on that server. `type` is passed straight through to
  the CLI's local `phelix <type> <app>` invocation, so it must match a
  command the CLI supports (currently: whatever `phelix` exposes as a
  subcommand — e.g. `restart`, `stop`, `start`). Sending an unsupported
  `type` results in a `MonitorCommandResult{status:"error"}`, not a crash.
  **The CLI pauses its own metrics tick while a command executes and force-
  sends a fresh snapshot immediately after**, so don't expect metrics during
  the ~2s a command is running.
- `strategy` / `replicas`: an optional **one-off deployment override** for a
  `rebuild` command — see [§2.4](#24-one-off-deployment-overrides).
- `ping`: a keepalive at the *application* level (independent of gRPC's own
  HTTP/2 keepalive pings — see §8). The CLI replies with a `Pong` carrying
  the same `id`. This exists mostly for backend-side liveness dashboards;
  it is not required for connection health (gRPC keepalive already covers
  transport-level dead-connection detection).

`request_id` should be a unique string per command (e.g. UUID) generated by
the backend; the CLI does not validate uniqueness, it only echoes it back.

### 2.4 One-off deployment overrides

`MonitorCommandRequest.strategy` and `.replicas` let the backend pick the
deployment path for a single `rebuild` without editing the app's
`phelix.yaml`. They are additive optional scalars: an agent built before they
existed ignores them, and a current agent that receives them unset behaves
exactly as it did before.

| `strategy` | `replicas` | What the agent runs |
|---|---|---|
| unset | unset | `phelix rebuild <app>` — strategy resolved from the app's `phelix.yaml`, classic if it declares none. Unchanged behavior. |
| `classic` | unset | `phelix rebuild <app> --strategy classic` — classic stop → build → start, even if `phelix.yaml` declares blue-green or rolling. |
| `blue-green` | unset | `phelix rebuild <app> --strategy blue-green` |
| `rolling` | unset | `phelix rebuild <app> --strategy rolling` — replica count from `phelix.yaml`'s `deploy.replicas`, else 1. |
| `rolling` | *N* | `phelix rebuild <app> --strategy rolling --replicas N` |

The override applies to that deployment only. The agent never writes it back
to `phelix.yaml`, so the next `rebuild` without an override returns to the
strategy the file declares.

**Rejections.** The agent will not silently substitute a different deployment
for one it cannot run exactly as asked. Each of the following is refused
before anything is built, as a `MonitorCommandResult{status:"error"}` whose
`error` names the problem:

- a `strategy` other than `classic`, `blue-green`, or `rolling`;
- `replicas` set on any strategy other than `rolling` (including with no
  `strategy` at all — the agent cannot know whether `phelix.yaml` says
  rolling);
- `replicas` less than zero;
- either field set on a command whose `type` is not `rebuild`. This is checked
  before dispatch, so an override on a `start`/`stop`/`restart`/`remove` is
  rejected rather than dropped.

A rejection leaves the application untouched: no build runs and no instance is
stopped.

---

## 3. Connection Lifecycle

```text
Client connects (grpc.Dial with TLS)
    ↓
Client opens MonitorStream, attaching auth metadata (see §4)
    ↓
Backend validates auth on the initial stream context
    ↓
Client immediately sends one MonitorEvent{server_info: ...}
    ↓
Backend registers/refreshes the server record
    ↓
Client sends a MonitorEvent batch (server_metrics, N×app_metrics,
N×app_info, 0+ log_entry) roughly every 2 seconds
    ↓
Backend updates server/app state per received event (see §6)
    ↓
Stream stays open indefinitely; backend may push MonitorControl at any time
    ↓
On disconnect (network blip, backend restart, CLI restart), the CLI
reconnects automatically with exponential backoff and re-sends server_info
as the first event of the new stream
```

Key implications for the backend:

- **Do not require** a separate "register server" RPC before `MonitorStream`
  — registration *is* the first `ServerInfo` event on the stream itself.
- **Treat every `ServerInfo` event as an upsert**, not a one-time
  registration. It arrives once per stream (re)connection, which can happen
  many times over a server's lifetime (deploys, network issues, backend
  restarts).
- The stream has no explicit "hello"/"ack" handshake beyond gRPC's own
  connection establishment + the auth metadata check. If auth fails, reject
  the stream immediately (see §4) rather than accepting it and erroring
  later.

---

## 4. Authentication

The CLI authenticates using the **same session** the rest of Phelix already
uses (stored client-side at `~/.phelix/session.json`, established via
`phelix auth login`). There is no separate credential system for monitoring.

On every `MonitorStream` call (and on every other CLI-initiated RPC), the
CLI attaches these gRPC metadata headers:

| Metadata key | Value | Notes |
|---|---|---|
| `authorization` | `Bearer <token>` | **Primary** — validate this like any other bearer-token API call. |
| `token` | `<token>` | Same token, unprefixed, for backends that read a raw header instead of parsing `Bearer`. |
| `sessionid` | `<session id>` | The CLI's local session identifier. |
| `serverid` / `x-server-id` / `server_id` | `<server id>` | The reporting server's stable ID, sent redundantly under three header spellings for backend compatibility. Prefer `server_id` field inside the message body as the authoritative value once the stream is established — the header is only needed to authenticate the *connection* before any message has been read. |

Backend implementation guidance:

1. Extract `authorization` (or `token`) from the incoming context via a gRPC
   **stream interceptor**, not inside the RPC handler — reject unauthenticated
   streams before `MonitorStream` is even invoked.
2. Validate the token exactly as you would for the existing REST API session
   validation (same backing session store). There is intentionally no new
   auth mechanism to maintain.
3. On invalid/expired/missing token: return `codes.Unauthenticated` and close
   the stream. Do **not** accept the stream and only fail later — the CLI
   treats a stream-open failure and a mid-stream auth failure identically
   (both trigger backoff+retry), so failing fast is strictly better for
   both sides.
4. If the token is valid but the caller lacks permission for the claimed
   `server_id` (e.g. token belongs to a different account), return
   `codes.PermissionDenied`.
5. **Never log the raw token or session ID.** The CLI already avoids logging
   credentials (see §9); the backend should do the same.

Credentials are sent once per stream (re)connection via metadata — **not**
repeated inside every `MonitorEvent` body. If the stream disconnects, the
CLI re-authenticates when it reconnects and opens a new stream.

---

## 5. Streaming

**`MonitorStream` is bidirectional streaming**, not unary and not purely
server- or client-streaming. This is intentional and mirrors the previous
WebSocket connection's nature:

- The CLI is predominantly the sender (metrics/logs/events flow
  client → server continuously).
- The backend occasionally needs to push data the other way (remote
  commands, pings) **without the CLI polling for it**, and without opening a
  second connection — the CLI is typically behind NAT/firewalls and only the
  CLI-initiated direction is guaranteed reachable.

**The CLI keeps exactly one long-lived stream open per server, for the
lifetime of the monitor daemon process.** It does **not**:

- open a new stream per metrics tick,
- open a new connection per metrics tick,
- poll or make repeated unary calls for monitoring data.

It sends a continuous sequence of `MonitorEvent` messages on the one stream,
roughly every 2 seconds, for as long as the daemon runs (hours to weeks).
Backend implementations must be designed for **long-lived stream handlers**
(one goroutine/task per connected server, not a request/response handler
pattern), and must not impose a short server-side stream deadline/timeout
that would kill healthy long-running connections.

---

## 6. Reconnection

The CLI reconnects automatically whenever the stream ends for any reason
(backend restart, deploy, network blip, load balancer idle-timeout, etc.),
using exponential backoff with jitter: 1s, 2s, 4s, 8s, 16s, 30s, 30s, ...
(capped at 30s, ±25% jitter). It does not "give up" — as long as the CLI
process is running, it keeps retrying indefinitely in the background, and
this never blocks or fails ordinary `phelix` CLI commands (see §9).

Because reconnection is automatic and can happen frequently in unstable
network conditions, the backend **must** treat the following as normal,
expected, and idempotent:

- **Stream disconnect** followed shortly by a **new stream** from the same
  `server_id` — this is not an error condition and should not be alerted on
  beyond normal connection-count metrics.
- **Duplicate server registration**: the same `ServerInfo` (same `id`)
  arriving repeatedly. Upsert by `id`; do not create duplicate server
  records.
- **Repeated application updates**: the same `app_id` reported repeatedly
  with possibly-unchanged data. Upsert by `(server_id, app_id)`; last write
  wins on `updated_at`/event timestamp.
- **State updates must be idempotent.** Applying the same `MonitorEvent`
  twice (e.g. due to a retry after a response was lost) must not corrupt
  state or double-count anything. Use `(server_id, app_id, timestamp)` or
  similar as a natural dedupe key if exactly-once semantics matter for your
  storage layer; at-least-once delivery is the model here, not
  exactly-once.

There is no "session resume" concept — every new stream starts fresh with a
new `ServerInfo` event, and the backend should treat it as the current
source of truth for that server going forward (not merge with stale data
from before the disconnect).

---

## 7. Message Semantics Summary

| Event | Backend action |
|---|---|
| `ServerInfo` | Upsert server record by `id`. Update "last seen" timestamp. When `connection`/`alert`/`security` are present, store them as the agent-reported settings defaults (see `docs/server-settings-backend.md`); nil groups leave existing settings untouched. |
| `ServerMetrics` | Append/update time-series metrics for the server. Update "last seen". |
| `AppResourceMetrics` | Append/update time-series CPU/RAM for `(server_id, app_id)`. |
| `ApplicationInfo` | Upsert application record by `(server_id, id)`. This is the source of truth for app status/PID/uptime — supersedes any prior record for the same app. |
| `MonitorLogEntry` | Append to the log store for `(server_id, app_id or self)`, keyed by `source`. Sort by `date`. `stream` (app logs) and `component` (self logs) are display/filter dimensions — see `docs/logging-backend.md`. |
| `MonitorCommandResult` | Correlate with the `request_id` of a previously-sent `MonitorCommandRequest`; update command/audit status; surface to whatever UI initiated the command. |
| `Pong` | Update liveness/RTT metrics for `id` (ping ID), if tracked. Not required for correctness. |

`MonitorControl.command` is backend-initiated: send it whenever an operator
(or automation) requests a lifecycle action on a managed app from your
control plane. For a `rebuild` it may carry `strategy`/`replicas` to pick the
deployment path once (§2.4); validate them before sending, because the agent
refuses rather than substitutes. `MonitorControl.ping` is optional and only
useful if you want an additional application-level liveness signal beyond gRPC
keepalive.

---

## 8. Error Codes

| Code | When the backend should return it |
|---|---|
| `Unauthenticated` | Missing/invalid/expired auth metadata on stream open (see §4). |
| `PermissionDenied` | Valid credentials, but the token's account doesn't own/can't act on the claimed `server_id`. |
| `InvalidArgument` | A `MonitorEvent` is structurally invalid — e.g. no oneof field set, or `server_id`/`timestamp` missing on a message that requires them. Prefer logging + dropping the single event over killing the whole stream if the rest of the stream is otherwise healthy, unless the client appears to be sending fundamentally malformed data. |
| `Unavailable` | Backend is shedding load, restarting, or a downstream dependency (DB, metrics store) is down. The CLI already treats this as retryable and backs off — this is the expected code for graceful backend restarts/deploys. |
| `ResourceExhausted` | Backend-side rate limiting / quota exceeded for this server or account. The CLI will back off and retry; make sure your limits are generous enough that a healthy single-server monitor daemon (~1 stream, ~1 event batch every 2s) never legitimately trips them. |
| `Internal` | Unexpected backend failure. The CLI retries with backoff; no special client-side handling, so don't rely on the client to work around backend bugs surfaced this way. |
| `Canceled` / `DeadlineExceeded` | Standard gRPC semantics — a canceled or timed-out call. The CLI treats these like any other stream-ending error (log + reconnect with backoff). Avoid imposing artificially short per-stream deadlines (see §5). |

The CLI never crashes or blocks other `phelix` commands on any of the above
— every monitoring error is logged to `~/.phelix/logs/phelix.log` and
retried in the background (see §9).

---

## 9. Client-Side Resilience (for backend context)

Not backend-actionable, but useful context for reasoning about backend load
and behavior:

- Monitoring runs only inside the `phelix monitor` foreground daemon. Other
  CLI commands (`phelix build`, `phelix status`, etc.) never open a
  `MonitorStream` and are completely unaffected if the backend is down.
- Failures are logged to `~/.phelix/logs/phelix.log`, not printed to the
  terminal, and never contain the auth token/session ID.
- Reconnection uses capped exponential backoff with jitter (§6); expect at
  most one active `MonitorStream` attempt per server at a time, never a
  tight retry loop.

---

## 10. TLS

**Production traffic must use TLS.** The CLI's transport selection is:

- Any `mode` other than `"dev"` in the CLI's config (i.e. the real-world
  default) → `credentials.NewTLS(&tls.Config{})` — standard TLS with server
  certificate validation against the system trust store. No client
  certificates, no custom CA pinning, no `InsecureSkipVerify`.
- `mode: "dev"` → plaintext (`insecure.NewCredentials()`), **only** intended
  for a developer running a local backend without TLS termination. This is
  never the default and is not used against the production `grpcUrl`.

**Backend requirement:** the production gRPC endpoint (`grpcUrl` in the
CLI's config, e.g. `phelix.anophel.com:50051`) must terminate TLS with a
certificate that validates against a public/standard CA trust store (i.e.
whatever the CLI's Go runtime trusts by default — a normal publicly-trusted
certificate is sufficient; no special pinning is required or supported on
the client side today). If you terminate TLS at a load balancer/reverse
proxy in front of the actual gRPC server, that's fine — just ensure it
proxies HTTP/2 correctly (gRPC requires HTTP/2, not HTTP/1.1).

There is no insecure fallback for production. If TLS negotiation fails, the
CLI's connection attempt fails and it retries with backoff — it does not
attempt a plaintext connection to the production URL.

---

## 11. Keepalive

The CLI configures gRPC transport keepalive as follows (client-side,
`google.golang.org/grpc/keepalive.ClientParameters`):

| Setting | Value |
|---|---|
| `Time` | 10s (ping the server if idle for this long) |
| `Timeout` | 3s (close the connection if no ack within this long) |
| `PermitWithoutStream` | `true` (keepalive pings are sent even with no active stream/RPC, since the connection is expected to be long-lived) |

**Backend/server-side requirement:** configure compatible keepalive
enforcement policy on the gRPC server (`keepalive.ServerParameters` /
`EnforcementPolicy`) that does not reject client pings at this cadence.
A reasonable, compatible server-side policy:

- `MinTime` no greater than ~10s (don't reject the client's ping interval).
- `PermitWithoutStream: true` if you want to allow keepalive pings between
  streams on the same connection (recommended, since the CLI may briefly
  have zero active RPCs between a stream ending and the next one starting).
- A `Time`/`Timeout` on the server side in the same ballpark (e.g. 10s/3s or
  slightly more relaxed) so the server also detects dead client connections
  without being trigger-happy over normal network jitter.

**Load balancer / proxy note:** if the gRPC endpoint sits behind an
L4/L7 load balancer or ingress, ensure it does not silently kill idle HTTP/2
connections faster than the keepalive interval above, and that it supports
long-lived HTTP/2 streams (many default ALB/ingress idle-timeout settings
are tuned for short-lived HTTP/1.1 requests and will need to be raised for
this workload).

---

## 12. Backward Compatibility

- **The WebSocket monitoring transport has been removed from the Phelix
  CLI.** There is no WebSocket server endpoint to maintain going forward for
  monitoring purposes; any existing WebSocket monitoring endpoint on the
  backend can be decommissioned once all deployed CLI versions have been
  upgraded to a version that uses gRPC (this document's contract).
- The gRPC monitoring endpoint (`PhelixService.MonitorStream`) is the sole
  monitoring transport going forward. There is no dual-transport or
  fallback mode in the CLI.
- The data model is unchanged from the WebSocket era — every field the old
  WebSocket JSON payloads carried (`servers`, `server_metrics`, `metrics`,
  `apps`, `app_logs`, `self_logs`, `command`/`command_response`) has a
  direct equivalent in the proto contract above. Backend teams migrating
  existing WebSocket-message handling logic should be able to map old
  handler-by-`type` logic onto the new oneof-by-payload-type logic roughly
  1:1.
- `MonitorCommandRequest.strategy` (4) and `.replicas` (5) were added after the
  initial contract. They are optional scalars on an existing message, so both
  directions degrade cleanly: an older agent ignores them and rebuilds from
  `phelix.yaml`, and an agent that supports them treats unset as "no override"
  — identical to the pre-field behavior. A backend that never sends them needs
  no change.
