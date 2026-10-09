# Execution Authorization

Phelix authorizes **execution** at a single boundary between a validated plan
and the deployment engine:

```text
request  →  plan  →  authorization  →  (approval)  →  execution
```

and never `request → execution` for a protected path. This is the layer that
makes Phelix safe to hand to automation: an agent decides *what* should be
deployed, Phelix decides *whether this caller may execute that exact plan*,
and a human decides *who is allowed to, and what needs signing off*.

> **Default behavior is unchanged.** A host that has not configured an
> authorization policy keeps working exactly as it did before — see
> [Local compatibility](#local-compatibility-the-transition-boundary).

## Authentication vs authorization

These are different questions, and Phelix keeps them apart on purpose:

| | Question | Status in Phelix |
|---|---|---|
| **Authentication** | Who is calling? | Not implemented for execution. A local `phelix` invocation is an **unauthenticated** local process, and Phelix says so. |
| **Authorization** | May this caller execute this plan? | Implemented — this guide. |

Phelix does **not** fabricate identity. There is no `--actor` flag, no
`--as` flag and no environment variable through which a caller can name
itself an authorized principal; such a value would be metadata, not proof,
and accepting it would mean any caller could grant itself anything.

`phelix auth login` is unrelated: it authenticates you to the **Phelix
dashboard backend** for monitoring. It grants no execution rights on the
host and is not consulted by the authorization boundary.

## The actor model

The boundary receives an abstract caller — never an HTTP header, a CLI flag
or MCP metadata:

```text
Caller  →  Authenticator  →  Actor  →  Authorization  →  Plan  →  Execution
```

An actor has three fields, matching the machine contract's `actor` block:

```json
{ "type": "cli", "id": null, "authenticated": false }
```

| Field | Meaning |
|---|---|
| `type` | `cli`, `api`, `agent`, `mcp` or `service` — the transport, not a privilege level |
| `id` | the established identity, or `null`; **only an authenticated actor ever has one** |
| `authenticated` | whether the identity was verified |

The invariant `id != null ⇒ authenticated` is enforced where actors are
validated, so an unauthenticated caller can never match a rule that names an
identity, however it invokes Phelix.

**Today there is exactly one authenticator**: the local CLI. It takes no
input, so there is nothing a caller can pass to influence it, and it always
reports `{type: "cli", id: null, authenticated: false}`.

What a local invocation *does* prove is write access to this host's Phelix
data directory. That is recorded as provenance (the host login name, in audit
records and approvals) but it is never an identity: no rule can match it, and
it grants nothing.

## Modes

Phelix has two postures, and the difference is the only thing an operator has
to understand.

### `legacy_local` — the trusted local path

The posture when **no policy file exists**. Local CLI execution is permitted,
exactly as in every Phelix release before this one. Non-CLI callers (`api`,
`agent`, `mcp`, `service`) are denied — those paths did not exist before and
must not inherit a trusted-by-default posture.

The `mcp` caller is produced by `phelix mcp serve` (the MCP adapter for coding
agents). To let an agent apply plans, add an `enforced` rule for the `mcp`
caller — ideally with `require_approval` — as described in
[the MCP guide](mcp.md#authorization-and-approvals).

### `enforced` — the protected machine path

Every deployment mutation must be **bound to an immutable plan** and
**permitted by a rule**. Nothing executes by default: no matching allow rule
means deny.

There is deliberately no third mode, no per-command mode and no "warn only"
mode: a boundary that can be observed but not enforced is not a boundary.

## Configuring the policy

The policy lives on the **host**, under the data directory:

```text
~/.phelix/authz/policy.json          # 0600; relocate with PHELIX_DATA_DIR
```

It is deliberately *not* in a project's `phelix.yaml`: an agent editing the
repository it deploys could otherwise rewrite the file that grants it
execution. Find the exact path on any host with:

```bash
phelix authz status
```

### Schema

```json
{
  "schema_version": "1",
  "mode": "enforced",
  "rules": [
    {
      "actor": { "type": "cli", "authenticated": false },
      "action": "rebuild",
      "target": "billing",
      "effect": "allow",
      "require_approval": true,
      "description": "local operator may deploy billing, with sign-off"
    }
  ]
}
```

| Field | Values |
|---|---|
| `actor.type` | `cli`, `api`, `agent`, `mcp`, `service`, or `*` |
| `actor.id` | an exact identity, or omitted for "any". Requires `authenticated: true` |
| `actor.authenticated` | **required, explicitly** — `true` or `false` |
| `action` | `rebuild`, `rollback`, or `*` |
| `target` | an application name, or `*` |
| `effect` | `allow` or `deny` |
| `require_approval` | `true` to additionally require a plan-bound approval |
| `description` | operator documentation; never evaluated |

Evaluation is fixed and total:

1. A malformed request or policy is `AUTHZ_INVALID` — fail closed.
2. An execution with no plan binding is denied.
3. **An explicit `deny` rule wins over any `allow` rule**, always.
4. An `allow` rule with `require_approval` is satisfied only by an approval
   bound to this exact plan.
5. Anything else is **denied by default**.

Wildcards are a bare `*` for a whole field. There is no globbing, no prefix
matching and no regular expressions, so a rule cannot match more than it
reads as matching.

`actor.authenticated` must be stated explicitly in every rule. A rule that
omits it is rejected rather than applied — "I forgot a field" must never
become "anyone may deploy".

### Why there is no policy language

Phelix does **not** implement Rego, CEL, embedded scripting or a custom
expression syntax, and does not plan to. The goal is an architectural
boundary that cannot be bypassed, not a policy programming language. If your
authorization needs exceed `actor × action × target`, you need a real IAM
system in front of Phelix — not a DSL inside it.

## What is protected

| Path | Gated |
|---|---|
| `phelix plan apply <plan-id>` | **yes** — plan-bound, fully authorized |
| `phelix rebuild` | **yes** — planless, so denied under enforcement |
| `phelix rollback` | **yes** — planless, so denied under enforcement |
| Webhook deployments | **yes**, transitively — the webhook queue deploys by re-invoking `phelix rebuild` |
| Remote rebuild (monitor daemon) | **yes**, transitively — same mechanism |
| Remote rollback (monitor daemon) | **yes** — gated in-process |
| `phelix rollback --dry-run` / `--list` | no — read-only |
| `phelix plan create` / `show` / `list`, `inspect`, `context`, `status`, `log` | no — read-only |
| `phelix build` | no — produces an artifact without serving it |
| `phelix start` / `stop` / `restart` | no — does not change the deployed version |

Under enforced authorization, a planless mutation is denied **even by the
most permissive possible rule set**: the plan binding is not a rule, it is
the boundary's shape. That closes the obvious bypass of "skip the plan, run
`rebuild` directly", and it is why `build`/`start`/`stop` being ungated is
not a hole in the deployment boundary — they cannot promote a new version.

Extending the boundary to the build and lifecycle commands is a later
phase's decision. It is listed here so the current edge is explicit rather
than implied.

## Plan binding

Authorization is never granted for a vague action like "deploy billing". It
is evaluated against the **immutable plan**, and the binding is both fields:

```text
plan_id    — WHICH artifact
plan_hash  — WHAT IT WILL DO
```

Authorizing by `plan_id` alone would let different content execute under an
old decision, which is exactly what Phase 3's content-addressed plans exist
to prevent. So:

- A decision for plan A can never permit plan B, even when the action and
  target are identical.
- A plan whose content hash differs from the one that was approved no longer
  matches that approval.
- There is no reusable "authorization token": the decision is re-evaluated
  from scratch, against the exact plan, every time execution is attempted.

## Approval

An `allow` rule with `require_approval: true` means: permitted, **once an
approval bound to this exact plan exists**.

```bash
phelix authz check   <plan-id>                        # what would the boundary say?
phelix authz approve <plan-id> --expect-hash sha256:… # record the approval
phelix plan apply    <plan-id>                        # now executes
phelix authz revoke  <plan-id>                        # remove the approval
```

`--expect-hash` asserts the hash you inspected; a mismatch is refused rather
than approved. **Machine callers should always pass it**, so an approval is
never granted to content the approver did not see.

### The approval artifact

```text
~/.phelix/authz/approvals/<plan-id>.json   # 0600, create-only
```

```text
approval
    ├── approval_id
    ├── plan_id      ─┐
    ├── plan_hash     │ the execution binding — all four must match
    ├── action        │
    ├── target       ─┘
    ├── approver (type, id, authenticated, source)
    ├── approved_at_ms
    └── approval_hash   (covers everything above)
```

An approval is **immutable**: the file is created exactly once, its content is
covered by a canonical hash that every load re-verifies, and re-approving a
plan is `ALREADY_EXISTS`. A changed decision is a revoke followed by a new
approve — never an edit.

Approvals are filed **by plan ID**, so a lookup for plan B physically cannot
return plan A's artifact; and the artifact's own `plan_id` must agree with
the file it was found under, so copying one into another plan's place is
`APPROVAL_INVALID` rather than a grant.

An approval may be reused across repeated applications of the **same** plan
(which do not re-execute anyway — see [Idempotency](#idempotency)). It is
never reusable for a different plan.

### Who the approver is

In this release the approver is recorded as:

```json
{ "approver_type": "cli", "approver_authenticated": false,
  "approver_source": "local_host" }
```

That is the honest statement of what was proven: **filesystem authority over
this host's Phelix data directory**. It is not dressed up as a verified
identity, and it is not derived from any caller-supplied name.

### No autonomous approval

Phelix has no code path that creates an approval as a side effect of
requesting execution. `phelix authz approve` is the only producer, and
`phelix plan apply` never calls it. An agent cannot approve its own action by
asking to execute it; whoever controls the host must perform the approval as
a separate, deliberate act.

### What approval is *not*

Deliberately absent, and not planned for this layer: email/Slack/web approval
flows, multi-person quorums, approval chains, escalation, notifications,
deadlines, expiry daemons and workflow DAGs. The primitive is
`this exact approver approved this exact plan` — enough to make "approval
required" a real execution gate instead of a label.

## Where the gate runs

```text
plan apply
  → load plan + verify hash          PLAN_CORRUPT / PLAN_INVALID / PLAN_HASH_MISMATCH
  → already applied?                 return the recorded correlation (no mutation)
  → evaluate preconditions           PLAN_STALE
  → evaluate capabilities            PLAN_CAPABILITY_MISSING
  → re-resolve the execution spec
  → verify plan/spec equivalence     PLAN_STALE
  → AUTHORIZE                        AUTHZ_* / APPROVAL_*      ← the boundary
  → request-key idempotency
  → execute
  → correlate operation
```

Two placements matter:

- **Authorization is the last gate before mutation.** When it runs, no
  operation record, no ledger entry and no engine call exists yet, so a
  refusal is provably a zero-mutation outcome. And because the decision is
  made at the instant mutation begins — inside the plan-application mutex —
  it cannot go stale between being made and being acted on.
- **Authorization runs *before* the request-key ledger.** A plan refused for
  want of an approval therefore consumes no idempotency key and applies
  cleanly once approved. Fail-closed must not mean "fail permanently".

Plan validity and authorization are independent, and **both** are required:

```text
plan valid + authorization granted  → may execute
plan stale + authorization granted  → PLAN_STALE,  zero mutation
plan valid + authorization denied   → AUTHZ_DENIED, zero mutation
```

A valid authorization never resurrects a stale plan.

## Local compatibility: the transition boundary

Introducing authorization changes nothing on a host that has not configured
it. The boundary between "unchanged" and "enforced" is a single file:

| Policy file | Posture |
|---|---|
| **absent** | `legacy_local` — existing local CLI behavior, preserved |
| present, `mode: legacy_local` | the same, stated explicitly |
| present, `mode: enforced` | the rule set decides; no match means deny |
| present, **unreadable** | `AUTHZ_UNAVAILABLE` — fail closed (retryable) |
| present, **invalid** | `AUTHZ_INVALID` — fail closed (not retryable) |

The last two rows are the production-safety property: once a host opts into
authorization, no form of brokenness degrades into "allow everything" — the
failure an unconfigured production target is most likely to hit. There is no
`missing config → allow` path for an enforcing host.

## Error codes

| Code | Exit | Retryable | Meaning |
|---|---:|---|---|
| `AUTHZ_DENIED` | 11 | no | This caller may not execute this. Do **not** retry. |
| `AUTHZ_UNAVAILABLE` | 11 | **yes** | The boundary could not be consulted (I/O). Nothing executed. |
| `AUTHZ_INVALID` | 11 | no | The host policy or the request is malformed. Needs an operator. |
| `APPROVAL_REQUIRED` | 11 | no | A plan-bound approval is required and none exists. |
| `APPROVAL_STALE` | 11 | no | An approval exists but its binding no longer matches this plan. |
| `APPROVAL_INVALID` | 11 | no | The approval artifact is corrupt or was edited after creation. |

Exit **11** uniformly means *the authorization boundary refused or could not
decide, and nothing mutated*. The JSON `error.code` distinguishes which, and
`error.retryable` says whether repeating could ever help. Plan staleness
keeps its own `PLAN_*` codes and exit **3**, so "not authorized" and "the
world moved on" never arrive as the same signal.

`AUTHZ_UNAVAILABLE` is the only retryable one: the policy may become readable
again. An agent must never be told to repeat a denied action, and an approval
requirement is not a transient failure.

## Audit and correlation

Every decision that gates an **execution attempt** — allow and refuse alike —
is appended to:

```text
~/.phelix/authz/decisions.jsonl    # 0600, bounded to the 512 newest records
```

```json
{"schema_version":"1","decision_id":"azd_…","decided_at_ms":0,
 "actor_type":"cli","actor_authenticated":false,"actor_provenance":"deploy",
 "action":"rebuild","target":"billing",
 "plan_id":"pln_…","plan_hash":"sha256:…",
 "decision":"allow","code":"AUTHZ_ALLOWED","mode":"enforced",
 "rule_index":0,"approval_id":"apr_…"}
```

This answers one question — *who was allowed or denied execution of this
exact plan, and when?* It is not a SIEM, an event bus or a general activity
log.

`phelix authz check` deliberately records **nothing**: it is a read-only
query, the log is bounded, and a probe must not be able to evict real
execution decisions. It runs the same evaluation an apply would, so its
answer still cannot disagree with one.

An authorized operation stores the decision that permitted it, closing the
chain in both directions:

```text
actor → authorization decision → approval → plan → operation → deployment
```

```bash
phelix operation status <op-id> --json
# → { plan_id, plan_hash, authz_decision_id, approval_id, deployment_id, … }
```

A decision record never references an operation: authorization happens before
the operation record exists, which is precisely why a refusal cannot leave
one behind.

## Security guarantees

What this layer does and does not promise:

- **Fail closed.** Every refusal path — denied, approval required, stale
  approval, invalid approval, unreadable policy, invalid policy, unresolvable
  caller — results in zero mutation. No code path returns "allowed" because
  something went wrong.
- **No mutation before authorization.** The gate precedes the operation
  record, the idempotency ledger and every engine call.
- **No forged identity.** Actors come only from an authenticator. An
  unauthenticated caller has no ID and cannot match an ID-bearing rule. No
  flag or environment variable can change the actor.
- **No secrets.** The authorization request carries metadata and references
  only — there is nowhere in it, in an approval, or in a decision record to
  put a password, token, key or environment value. Free-text fields pass
  through the existing centralized redactor; no new secret-handling
  mechanism was introduced.
- **No bypass through another command.** Every path that can promote a
  deployed version terminates at this gate, including the webhook and remote
  command paths (which re-invoke the gated commands).
- **Audit cannot alter a decision.** A failed audit write is reported, never
  obeyed: it neither blocks an authorized execution nor rescues a denied one.

The honest limit: **the data directory is the trust boundary.** Approval
hashes detect corruption and casual tampering, but they are not signatures —
this release has no key material, and inventing one would be fake
cryptographic authentication. Anyone who can write `~/.phelix/` can equally
rewrite `apps.json` or the plan store, so protect that directory at the OS
level (see [Security](security.md)).

## Idempotency

Phase 3 idempotency is untouched. Plan application still derives the request
key `pln:<plan-id>` with the plan hash as its fingerprint, and:

- **Repeat after success** returns the recorded correlation
  (`already_applied: true`) — never a second mutation. This happens *before*
  authorization, so editing a policy or revoking an approval after an
  execution does not retroactively change what Phase 3 reports about it.
- **Concurrent apply** serializes; exactly one execution, and exactly one
  recorded allow decision.
- **After a restart**, an interrupted key replays its indeterminate result and
  the plan is never re-executed — approved or not.
- **A refusal consumes no key**, so the same plan applies once authorized.

## Future authentication

The integration point is the `Authenticator` seam, and nothing below it needs
to change:

```text
Caller  →  Authenticator  →  Actor  →  Authorization  →  Plan  →  Execution
            ▲
            └── a future phase implements this for API credentials, service
                identity, an MCP session or an agent credential
```

An authenticated non-CLI caller is authorized by the **same rules** through
the **same gate** — write `{"actor": {"type": "mcp", "id": "mcp-1",
"authenticated": true}, …}` and it works the moment an authenticator can
produce that actor.

**Not implemented today, and labelled as future throughout:** real
authentication of any caller, API/MCP front ends, OAuth/OIDC/SAML, an agent
subsystem, roles or groups, and approval workflows. Phelix contains no MCP
server and no `phelix agent` command; the architecture simply does not need
redesigning to gain them.

## Related

- [Machine contract](../reference/machine-contract.md) — the `--json` envelope
  and the Phase 4 command surface.
- [Exit codes](../reference/exit-codes.md), [Error codes](../reference/error-codes.md).
- [Security](security.md) — secrets at rest and the `~/.phelix/` permission
  model.
- [Data directory](../reference/data-directory.md) — where the policy,
  approvals and decision log live.
- [Authentication](authentication.md) — the **dashboard** session, which is a
  different thing entirely.
