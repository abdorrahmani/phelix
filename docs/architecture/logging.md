# Logging pipeline

Phelix produces three kinds of log records: its own daemon logs, managed
application output, and proxy access logs. Every record is written to disk as
one JSON object per line and shipped to the backend over the monitor stream.
This page explains how self logs travel and what the backend must do with
them.

## Self logs on disk

The daemon writes to `phelix.log` under the Phelix data directory, normally
`~/.phelix/logs/phelix.log`. `phelix log` with no argument reads this file.
Rotated files are named `phelix.log.1`, `phelix.log.2`, and so on. Older
rotations may be compressed as `.gz`.

A self record looks like this:

```json
{
  "ts": "2026-10-31T18:04:05.123Z",
  "level": "warn",
  "source": "self",
  "component": "grpc",
  "msg": "monitor stream disconnected: EOF",
  "fields": {"attempt": "3"}
}
```

Levels are lowercase: `debug`, `info`, `warn`, `error`. The `component` field
names the subsystem, for example `grpc`, `monitor`, `deploy`, `health`,
`logs`, or `misc`. Values in `msg` and `fields` are redacted before they reach
the file or the wire. Field names that look like credentials are dropped.

## Collection and delivery

The monitor daemon collects self logs on the normal monitor tick, every 2
seconds, while at least one app is watched. A server with every app unwatched
sends no server data at all. Collection reads the active file and any plain
rotations, keeps a per-file cursor, and does not re-read lines it has already
sent. On a cold start it reads the last five lines of each file.

Self logs are sent on both log channels during the current transition:

- `MonitorEvent.log_entry` with payload field 14, one event per line.
- `MonitorEvent.log_batch` with payload field 22, up to 200 entries or 500 KB
  per batch, flushed at least every 200 ms.

The batch queue is bounded at 5000 pending entries. Overflow is counted in
`MonitorLogBatch.dropped` and the affected lines are discarded.

## Wire fields

Self entries use the same `MonitorLogEntry` message as app entries. The table
below lists what is set for source `self`.

| Field | Value for self logs |
|-------|---------------------|
| `id` | Server id |
| `server_id` | Server id |
| `app_id` | Empty |
| `log` | The message |
| `date` | Record time in Unix milliseconds |
| `level` | Lowercase level |
| `source` | `LOG_SOURCE_SELF` |
| `stream` | `LOG_STREAM_UNSPECIFIED` |
| `component` | Subsystem that wrote the line |
| `app` | Empty |
| `version` | Serving version when known |
| `deploy_id` | Reserved, currently empty |
| `request_id` | Request id when the line is request scoped |
| `fields` | Extra structured values, already redacted |

The daemon declares the `log_batch` capability in `CLIMetadata.capabilities`.
Older backends can ignore the unknown oneof case and keep reading the legacy
per-line events.

## Backend changes

The backend needs to treat `source = LOG_SOURCE_SELF` as a server log stream,
not as an app log with a missing app id.

1. Accept `MonitorLogBatch` payload field 22 from the monitor stream.
2. Store self rows with `server_id`, `date`, `level`, `component`, `log`, and
   `fields`. Do not require `app_id` or `app` for these rows.
3. Group the dashboard view by `component`, and show self logs in a server
   section rather than inside an application section.
4. Deduplicate the legacy per-line stream against the batch stream. A stable
   key during the transition is `(server_id, date, log, request_id)`, with
   `component` added when the same message can repeat in the same
   millisecond.
5. Apply the same retention and search rules used for app logs. The new
   fields `app`, `version`, `deploy_id`, `request_id`, and `fields` are already
   present on the wire.
6. Keep treating `fields` as untrusted user data. The agent redacts values and
   drops sensitive field names, but the backend is still the last boundary.

No extra RPC is needed. Self logs use the existing `MonitorStream` call and
the existing `MonitorEvent` envelope.

## Check before reporting a missing stream

- Confirm that at least one app is watched. `phelix watch <app>` enables the
  gate.
- Confirm that `phelix monitor` is running.
- Confirm that `phelix log` shows new lines. If it does, the file and the
  collector are working.
- Check the log file path. A service started with a different `HOME` or
  `PHELIX_DATA_DIR` writes to a different directory.
- Wait at least one monitor tick. Self logs ship every 2 seconds now, not only
  on the slower host metrics interval.
