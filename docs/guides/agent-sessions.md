# Agent Execution Sessions

A **session** is a durable, auditable record that groups **one unit of agent
work** — the arc from inspecting the environment, through planning, authorizing
and applying a change, to verifying the result:

```text
Agent → Session → Context/Inspect → Plan → Authorization/Approval → Apply → Operation/Deployment → Verify → Session completion
```

Phelix is the controlled execution layer for coding agents. A session is the
**tracking primitive** that lets an agent (or a *different* agent, after a
restart) answer "what work was in flight, what did it touch, and where did it
get to?" by reading durable state — without Phelix ever deciding or executing
the next step for it.

A session is deliberately **not**:

- an execution engine — it runs nothing;
- an authorization principal — creating or modifying a session grants no
  permission, and a session id is an identifier, never a credential;
- a second source of truth — it references plans, operations and deployments by
  id and never copies their authoritative state.

## How a session differs from a plan, operation or deployment

| Concept | Owns | Lifecycle | Mutates the host? |
|---|---|---|---|
| **Plan** | an immutable, content-addressed description of one mutation | `created → applied\|failed` | only via `plan apply`, behind the authorization boundary |
| **Operation** | the durable record + idempotency of one executed mutation | `pending → running → succeeded\|failed` | it *is* the record of a mutation |
| **Deployment** | the live blue-green/rolling topology of an app | deploy engine state | yes — the running system |
| **Session** | references to the above + a bounded checkpoint/event trail | `active → completed\|failed\|cancelled` | **never** |

Closing a session (complete/fail/cancel) **does not** stop a deployment, cancel
an operation, or change an application. It ends the *tracking workflow*, nothing
else.

## Lifecycle

```text
create                         → active
active   --checkpoint-------->   active        (record a step and/or link references)
active   --complete-------->     completed
active   --fail----------->      failed
active   --cancel--------->      cancelled
{completed, failed, cancelled} → (terminal — no transition; never re-activated)
```

Rules enforced in code:

- A mutation on a terminal session is refused with `SESSION_INVALID_TRANSITION`
  and the record is left exactly as it was.
- `completed` means **the tracking workflow finished** — it is a *reported*
  outcome, **not** a verified statement about runtime health. Establish health
  independently (`phelix operation status`, `phelix inspect deployment|health`).
- `completed` ≠ a deployment succeeded, and a failed operation does **not**
  force the session to `failed`: a session can stay `active` while an operation
  is running or indeterminate, and the agent decides what to do next.
- References may be attached while `active`; `complete`/`fail` may record final
  references as part of that terminal write. After a terminal state, reads only.

## Schema

A session is stored as one JSON file at `<data-dir>/sessions/<session-id>.json`
(`0600`), serialized deterministically.

```json
{
  "schema_version": "1",
  "session_id": "ses_1a2b3c4d5e6f7081",
  "status": "active",
  "title": "ship billing v3",
  "app": "billing",
  "project": "acme",
  "actor": { "type": "cli", "id": null, "authenticated": false },
  "rev": 3,
  "created_at_ms": 0,
  "updated_at_ms": 0,
  "plan_ids": ["pln_…"],
  "operation_ids": ["op_…"],
  "deployment_ids": ["dep-…"],
  "checkpoint": { "step": "deploy", "note": "canary at 10%", "at_ms": 0, "seq": 2 },
  "final_result": "shipped v3 to 100%",
  "events": [
    { "seq": 0, "type": "created", "at_ms": 0, "detail": "session created" },
    { "seq": 1, "type": "checkpoint", "at_ms": 0, "detail": "step=plan; linked pln_…" }
  ]
}
```

- **`session_id`** — `ses_` + 16 hex chars, minted at creation, durable.
- **`actor`** — provenance only (`cli`, or `mcp` under `phelix mcp serve`),
  derived server-side from the same authenticator the authorization boundary
  uses. It is **never** taken from client input and is **never** a privilege.
- **`rev`** — a monotonic revision bumped on every successful change; the
  optimistic-concurrency token for `--if-rev`.
- **`events`** — the append-only history (one event per mutating command),
  ordered by a monotonic `seq` assigned under the per-session lock. This is
  honest provenance under the data-directory trust boundary — **not** a
  cryptographically authenticated audit log (there is no key material).

Bounds (over-cap input is **rejected** with `INVALID_ARGUMENT`, never silently
truncated): title/project ≤ 200, step ≤ 120, note ≤ 500, final result ≤ 2000,
≤ 50 references of each kind, ≤ 200 events, ≤ 256 KiB total.

## CLI

Every command accepts `--json` and emits the standard machine envelope.

```bash
phelix session create --title "ship billing v3" --app billing --json
phelix session show ses_1a2b3c4d5e6f7081 --json
phelix session list --status active --app billing --limit 20 --json
phelix session checkpoint ses_… --step deploy --note "canary at 10%" \
    --plan pln_… --operation op_… --deployment dep-… --json
phelix session complete ses_… --result "shipped v3 to 100%" --json
phelix session fail ses_… --reason "canary regressed; rolled back" --json
phelix session cancel ses_… --reason "superseded by a newer plan" --json
```

`checkpoint`/`complete`/`fail`/`cancel` accept `--if-rev N`: the change is
applied only if the session is still at revision `N`, otherwise it is refused
with `SESSION_CONFLICT` (retryable — re-read and retry). Omit it to let
concurrent updates serialize and all land. `cancel` ends tracking only; it is
distinct from `complete` (success) and `fail` (a reported failure) and performs
no execution-layer action.

## JSON output and error codes

A session is its own `ses_` artifact, not an operation, so the success envelope
is read-shaped — `{schema_version, status:"succeeded", result:{…session…}}`
with **no** `operation_id` (like `plan create`). `session show` adds a
`resolved` block (see below). `session list` returns
`{sessions:[…], count, truncated}`.

| Code | Exit | Retryable | Meaning |
|---|---:|---|---|
| `NOT_FOUND` | 12 | no | no session with that id |
| `INVALID_ARGUMENT` | 2 | no | malformed id, invalid/absent referenced entity, invalid checkpoint, or over-cap text |
| `SESSION_INVALID` | 3 | no | a stored record has a foreign schema, malformed id, or unknown status |
| `SESSION_CORRUPT` | 3 | no | a stored record is not parseable |
| `SESSION_INVALID_TRANSITION` | 3 | no | the lifecycle transition is illegal from the current state (zero mutation) |
| `SESSION_CONFLICT` | 1 | **yes** | an `--if-rev` optimistic-concurrency check failed; re-read and retry |
| `ALREADY_EXISTS` | 1 | no | session-id collision on create (astronomically unlikely) |

All free text passes the shared redactor **before persistence**, so neither the
on-disk record nor any output can carry a secret.

## Linking sessions to existing work

A session correlates existing entities by id; it never owns their state.

- `--plan pln_…` and `--operation op_…` are verified to **exist** at link time
  (a nonexistent or malformed reference is `INVALID_ARGUMENT`); a plan is also
  hash-verified. Linking a plan **does not authorize it**.
- `--deployment dep-…` is format-validated at link time. Phelix keeps no
  standalone deployment-id index, so a deployment reference's existence is
  resolved at `show` time via its correlating operation record.

`session show` resolves every reference through the existing read APIs and
reports each honestly in `resolved`, never fabricating state and never failing
the whole show on one bad reference:

```json
"resolved": {
  "plans":       [{ "id": "pln_…", "present": true,  "state": "present", "status": "applied", "detail": "sha256:…" }],
  "operations":  [{ "id": "op_…",  "present": true,  "state": "present", "status": "succeeded", "detail": "rebuild" }],
  "deployments": [{ "id": "dep-…", "present": false, "state": "unknown" }]
}
```

Reference states: `present` (read successfully), `missing` (does not exist —
e.g. pruned), `unavailable` (exists but could not be read — corrupt/tampered,
with `error_code`), `unknown` (a deployment id with no correlating operation —
reported honestly rather than invented).

## Recovery after an interruption

The point of a durable session is that a restarted agent can resume by
**reading**. Phelix never re-runs an interrupted plan or infers that an
incomplete operation failed. A new agent should:

1. `phelix session list --json` — find sessions.
2. `phelix session show <id> --json` — read status, checkpoint and references.
3. `phelix plan show <plan-id> --json` — re-verify each linked plan's current
   applicability (it may now be stale).
4. `phelix operation status <op-id> --json` — read each linked operation's real
   status (succeeded / failed / running / indeterminate).
5. `phelix inspect deployment|health <app> --json` — check live state.
6. Decide, from those facts, what to do next — Phelix does not decide for it.

Because every CLI invocation is a fresh process, re-reading a session is
literally cross-restart recovery, and it executes nothing.

## Authorization and security

- **A session grants nothing.** Creating or modifying a session, or linking a
  plan to it, confers no execution right. The only path that executes is
  `plan apply`, through the unchanged Phase 4
  [authorization boundary](authorization.md): an approval-required plan still
  returns `APPROVAL_REQUIRED` after it is referenced by a session.
- **No forged identity.** The actor is produced by the authenticator, not from
  tool inputs or flags; there is no `--actor`. The MCP caller records `mcp`, the
  CLI records `cli`, and the two never merge — a session never grants the MCP
  caller the local CLI's legacy trust.
- **Fail closed.** Missing, malformed and corrupt records fail with the codes
  above and zero mutation; invalid transitions leave the record untouched.
- **No secrets.** Free text is redacted before persistence; a session record has
  no field for an env value, token or key.
- **Trust boundary.** As with Phase 4 approvals, the data directory is the trust
  boundary. The event history is append-only and ordered, but it is **not**
  tamper-proof — anyone who can write `~/.phelix/` can rewrite it. Protect that
  directory at the OS level (see [Security](security.md)).

## MCP tools

The [MCP adapter](mcp.md) exposes six session tools that reuse the exact same
command logic as the CLI, over local stdio:

| Tool | Mutates? |
|---|---|
| `phelix_session_create` | writes a tracking record |
| `phelix_session_show` | no |
| `phelix_session_list` | no |
| `phelix_session_checkpoint` | writes a tracking record |
| `phelix_session_complete` | writes a tracking record |
| `phelix_session_fail` | writes a tracking record |

`phelix_session_fail` is included so an agent can honestly record a non-success
outcome rather than mislabel work `completed`. Session **cancellation** and the
`--if-rev` optimistic-concurrency primitive are CLI-only (a deliberate operator
action and a scripting primitive, respectively). None of these tools executes,
authorizes, or mutates a deployment, and an MCP-created session records `mcp`
provenance automatically.

## Limitations

- Sessions track; they do not orchestrate. There is no scheduler, no automatic
  advancement, no retry/replay of a mutation, and no autonomous resume.
- No cross-host or multi-agent session coordination; storage is the local data
  directory only.
- Deployment references are format-validated and resolved via operation
  correlation, not against a dedicated deployment-id store (there is none).
- The audit trail is honest provenance, not cryptographically signed.

## Related

- [Machine contract](../reference/machine-contract.md) — the `--json` envelope.
- [Authorization](authorization.md) — the gate a session can never bypass.
- [MCP adapter](mcp.md) · [Exit codes](../reference/exit-codes.md) ·
  [Error codes](../reference/error-codes.md) ·
  [Data directory](../reference/data-directory.md).



