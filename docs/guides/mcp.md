# MCP Adapter (Model Context Protocol)

Phelix ships a built-in **MCP server** that exposes a small, deliberately
bounded slice of its existing machine interface to autonomous coding agents
(Claude Code, Codex, and any MCP-compatible client) over **local stdio**.

Phelix is the controlled execution layer for coding agents: the agent reasons
and proposes; Phelix provides *controlled* execution, deployment, observation
and recovery. The MCP adapter is a **transport boundary only** — it adds no new
execution engine, no AI, and no network listener. Every tool call runs the
exact same validation, plan, authorization, approval and idempotency pipeline
as the equivalent `phelix` CLI command, and returns the same versioned machine
envelope (`docs/reference/machine-contract.md`).

```
AI client ──stdio(JSON-RPC)──▶ phelix mcp serve ──▶ existing Phelix pipeline
                                 (internal/mcp)        (plans · authz · ops · deploy)
```

## Scope

In scope (this phase): a local stdio server exposing read tools (context,
inspect, plan show/list, operation status) and two controlled mutation tools
(plan create, plan apply).

Explicitly **out of scope**: any LLM or embedded agent; network/remote MCP;
RBAC or a policy DSL; new approval workflows; a new deployment engine; a
`phelix agent` hierarchy; direct rebuild/rollback/deploy/approval/shell/filesystem
tools that would bypass plan-based authorization.

## Prerequisites

- A `phelix` binary (see `docs/getting-started/installation.md`).
- An MCP-capable client. Verified formats below: **Claude Code** and **Codex**.
- The server inherits the launching user's OS permissions and reads/writes the
  normal Phelix data directory (`~/.phelix`, or `$PHELIX_DATA_DIR`;
  `docs/reference/data-directory.md`). It needs no network and opens no socket.

## Starting the server

```bash
phelix mcp serve --stdio
```

- **stdout carries MCP JSON-RPC only.** All diagnostics and logs go to stderr.
  Never pipe anything else into this process's stdout.
- `--stdio` is the only supported transport. There is no network listener.
- The server runs until the client disconnects (stdin EOF) or it receives
  SIGINT/SIGTERM, then shuts down cleanly.

You normally do **not** run this by hand — your agent launches it for you (next
section).

## Configure your agent

Use the **absolute path** to the `phelix` binary (`which phelix`). The server's
working directory is whatever the agent launches it in; the composite
`phelix_context` read reflects that directory, so launch the agent from your
project root if you want project context. Pass environment through the client's
`env` block — e.g. `PHELIX_DATA_DIR` to point at a non-default data directory.
Never put real secrets in these files.

### Claude Code

CLI:

```bash
claude mcp add phelix -- /usr/local/bin/phelix mcp serve --stdio
```

or `.mcp.json` (project- or user-scoped):

```json
{
  "mcpServers": {
    "phelix": {
      "command": "/usr/local/bin/phelix",
      "args": ["mcp", "serve", "--stdio"],
      "env": { "PHELIX_DATA_DIR": "/var/lib/phelix" }
    }
  }
}
```

### Codex

Add a table to `~/.codex/config.toml` (or run `codex mcp add`):

```toml
[mcp_servers.phelix]
command = "/usr/local/bin/phelix"
args = ["mcp", "serve", "--stdio"]
# env = { PHELIX_DATA_DIR = "/var/lib/phelix" }
```

### Verifying

- With the official **MCP Inspector**: point it at the command above; it should
  list the seven `phelix_*` tools and let you call `phelix_inspect`
  `{"resource":"capabilities"}`.
- From the client: ask it to list tools, then call `phelix_context`. A clean
  JSON envelope with `"status": "succeeded"` confirms the wiring.
- The repository's `cmd/mcp_integration_test.go` starts the real binary over
  stdio and exercises the full workflow; run it with `go test ./cmd/ -run
  TestMCPStdioServer`.

## Tools

| Tool | Mutates? | Required args | Optional args |
|---|---|---|---|
| `phelix_context` | no | — | `app`, `versions` (≤100), `operations` (≤100), `log_lines` (≤1000) |
| `phelix_inspect` | no | `resource` | `app` (required for app/deployment/versions/health), `limit` (≤100) |
| `phelix_plan_show` | no | `plan_id` | — |
| `phelix_plan_list` | no | — | `app`, `limit` (default 20, ≤100) |
| `phelix_operation_status` | no | `operation_id` | — |
| `phelix_plan_create` | writes a plan artifact only | `action` (`rebuild`\|`rollback`), `app` | rebuild: `port`, `strategy`, `blue_green`, `replicas`, `canary`, `tag`, `auto_rollback`, `no_upload` · rollback: `to`, `verify`, `reason` |
| `phelix_plan_apply` | **yes** (executes) | `plan_id` | — |

`phelix_inspect` resources: `project`, `runtime`, `config`, `capabilities`,
`operations`, `app`, `deployment`, `versions`, `health`.

**Two-step mutation.** `phelix_plan_create` only *describes* and persists an
immutable, content-addressed plan (hash included) — it never executes.
`phelix_plan_apply` executes a previously created plan, and only after it passes
the full pipeline. There is no single-step "deploy" or "rollback" tool, by
design.

### Result contract

Every tool returns exactly one JSON document — the Phelix machine envelope — as
the result's text content (and mirrored into `structuredContent`):

```json
{ "schema_version": "1", "operation_id": "op_…", "status": "succeeded", "result": { … } }
```

- `status` is one of `pending | running | succeeded | failed | cancelled`.
- A **Phelix failure is a normal result**, not a protocol error: the envelope
  carries `"status":"failed"` with a stable `error.code`, `error.exit_code` and
  `error.retryable`, and the tool result's `isError` flag is set. Authorization
  denials, approval-required, stale/corrupt plans, and build/deploy failures all
  arrive this way — never silently converted to success.
- Only genuine invocation problems (unknown tool, schema-invalid arguments,
  unknown `resource`, malformed id) surface as a tool/transport error.
- `operation_id` and plan/deployment/authz correlation are preserved exactly as
  in the CLI envelope, so an agent can join a plan → operation → deployment.

## Authorization and approvals

The MCP server authorizes as a distinct, **unauthenticated local MCP caller**
(actor type `mcp`), never as the local CLI. This is the whole security posture
of the adapter, and it has one consequence worth stating plainly:

> On a host that has **not** configured authorization (the default,
> `legacy_local` mode), MCP read and plan-create tools work, but
> **`phelix_plan_apply` is denied** (`AUTHZ_DENIED`). Only the local CLI inherits
> legacy-local trust; a new transport must not. The adapter can therefore never
> gain more privilege than the local CLI, and by default has less.

To let an agent execute plans, the host operator opts in with an **enforced**
policy that allows the `mcp` caller — written to `~/.phelix/authz/policy.json`
(never in a repository the agent can edit). Requiring a plan-bound approval is
strongly recommended:

```json
{
  "schema_version": "1",
  "mode": "enforced",
  "rules": [
    {
      "actor": { "type": "mcp", "authenticated": false },
      "action": "*",
      "target": "my-app",
      "effect": "allow",
      "require_approval": true
    }
  ]
}
```

With `require_approval: true`, the flow is:

1. Agent calls `phelix_plan_create` → gets a `plan_id` and `plan_hash`.
2. Agent calls `phelix_plan_apply` → returns `status:"failed"`,
   `error.code: APPROVAL_REQUIRED`. Nothing executed.
3. A human approves **that exact plan** out of band:
   `phelix authz approve <plan-id>`.
4. Agent calls `phelix_plan_apply` again → the boundary allows it and execution
   proceeds, correlated to an operation id.

Approvals bind the plan id **and** content hash: an approval for one plan can
never authorize another, and a plan whose content changed no longer matches its
approval. Approval creation and revocation are **not** MCP tools — they are
human actions (`phelix authz approve|revoke|check|status`). See
`docs/guides/authorization.md`.

## Security and trust boundaries

- **Local boundary, not authenticated remote access.** stdio MCP trusts the OS:
  any process able to launch `phelix` as your user — including a compromised
  agent — can call these tools with your permissions, exactly as it could run
  `phelix` directly. The honest trust boundary is write access to the Phelix
  data directory, the same as the CLI.
- **No caller-supplied identity.** Tool inputs carry no actor, role,
  authenticated flag, approval or authorization field, and the server ignores
  any such value — the authenticator is the only producer of an actor. A client
  connecting is never treated as proof of identity or authorization.
- **Fail-closed and integrity-preserving.** Plan hash verification, stale-plan
  and capability checks, the authorization boundary, plan-bound approvals and
  request-key idempotency are all preserved; a refused or interrupted apply
  mutates nothing and is never silently retried into execution.
- **Redaction and bounds.** Every envelope passes the shared secret redactor;
  all list/limit arguments are bounded, and a single JSON-RPC frame is capped.

## Troubleshooting

- **Client reports a protocol/parse error on startup** — something wrote to
  stdout before/around the protocol. Ensure nothing wraps the command and that
  you launch `phelix mcp serve --stdio` directly; diagnostics belong on stderr.
- **`phelix_plan_apply` returns `AUTHZ_DENIED`** — the host has no policy
  allowing the `mcp` caller. Add an enforced policy as above (`phelix authz
  status` shows the current posture).
- **`APPROVAL_REQUIRED`** — approve the exact plan: `phelix authz approve
  <plan-id>`, then re-apply.
- **Invalid tool arguments / unknown resource** — the tool returns a clear
  error; check the `resource` enum and id formats (`pln_…`, `op_…`).
- **Server exits immediately** — run `phelix mcp serve --stdio` in a terminal
  and read stderr; a config or data-directory error is reported there.

## Limitations

- Local stdio only; no remote/network transport.
- `phelix_plan_create` deliberately omits filesystem/build-arg inputs
  (`--source-dir`, `--build-arg`): no arbitrary paths or build arguments over
  MCP. Create such plans with the CLI if needed.
- Approval management is a human/CLI action, never an MCP tool.
- Matrix/webhook/dockerize and process-lifecycle commands are not exposed as
  MCP tools in this phase (their state is still observable via
  `phelix_operation_status` and `phelix_inspect`).



