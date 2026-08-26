# Phelix App Removal — Backend Integration Guide

**Audience:** backend engineers implementing/maintaining the Phelix backend
gRPC server. Assumes no familiarity with the Phelix CLI codebase.

**Status:** the `remove` lifecycle action is now fully wired in both
directions:

1. **CLI-initiated** — `phelix remove <ID|AppName>` reports a
   `ReportEvent` with `action="remove"` to the backend (this existed before).
2. **Backend-initiated** — the backend can push a remote command
   `type="remove"` over `MonitorStream`; the CLI removes the managed app
   in-process (this is new).

---

## 1. Backend-initiated removal (`MonitorStream`)

Send a `MonitorControl` on the stream with a `MonitorCommandRequest`:

```proto
message MonitorCommandRequest {
  string request_id = 1;
  string type = 2;       // "remove" — now supported alongside start/stop/restart
  string app_name = 3;    // app name OR app ID — CLI resolves either
}
```

Example payload the backend emits:

```json
{
  "command": {
    "request_id": "<uuid>",
    "type": "remove",
    "app_name": "my-app"
  }
}
```

### What the CLI does on receipt

- Resolves `app_name` by **name or ID** against local state (`apps.json`).
- Executes removal **in-process** through the app manager (no `phelix remove`
  subprocess, so it works under systemd where PATH may be limited):
  1. Stops the process first if the app is running.
  2. Deletes the app binary (`app_<id>` in the app directory) and log file.
  3. Removes the app entry and persists state.
- Replies with a `MonitorEvent{command_result}`:

```proto
message MonitorCommandResult {
  string request_id = 1;  // echoes your request_id
  string command = 2;     // "remove"
  string app_name = 3;
  string status = 4;      // "success" | "error"
  string error = 5;       // populated iff status == "error"
  int64 timestamp = 6;
}
```

Error cases surfaced through `status:"error"` (never a crash): unknown app,
process stop failure, file deletion failure, state save failure.

### Observing the removal

While the command executes (~2s), metrics ticks are paused. Immediately after
completion the CLI force-sends a fresh snapshot — the removed app will no
longer appear in `apps`/`metrics` payloads. Don't treat its disappearance
from the tick *during* execution as authoritative; wait for the
`MonitorCommandResult`.

---

## 2. CLI-initiated removal (`ReportEvent`)

When a user runs `phelix remove` locally, the CLI reports:

```proto
message ApplicationEvent {
  string server_id = ...;
  string app_id = ...;
  string app_name = ...;
  string action = "remove";
  bool   success = ...;
  string error_message = ...;  // populated iff success == false
  int64  timestamp = ...;
  // pid / deployment_mode / version are zero/empty for remove
}
```

sent via `PhelixService.ReportEvent`, exactly like `start`/`restart` events.
The event fires once per removal attempt: `success=true` on success,
`success=false` with `error_message` on failure (removal aborts before any
destructive step if stopping the running process fails).

---

## 3. Notes

- `request_id` should be unique per command (e.g. UUID); the CLI only echoes
  it back.
- Removal is destructive and irreversible on the agent host — gate it behind
  whatever confirmation your backend UI already uses for other destructive
  operations.
- No proto changes are required: `remove` rides the existing
  `MonitorCommandRequest.type` string field.
