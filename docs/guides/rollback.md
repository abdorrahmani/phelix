# Rollback

Phelix keeps every successful build as a numbered version, so you can revert to
any retained version. `phelix rollback` automatically chooses the right
strategy: classic stop→start for plain builds, or zero-downtime for apps
deployed with blue-green/rolling.

> This guide merges what the README previously covered in two separate places
> ("Versioning & rollback" and "Versioned builds and rollback"). All commands,
> flags, safety guarantees, and examples from both are preserved here.

## Versioned builds

Every successful build — whether via `phelix build`, `phelix rebuild`, or
zero-downtime deploy — creates a numbered version (`v1`, `v2`, `v3`, …) rather
than overwriting. Versions store both the binary and its paired encrypted env
snapshot, so rollback always restores a known-good binary + env pair — never
binary-only.

### Build-and-deploy ordering guarantee

Versions use a two-phase commit to ensure safety:

1. **Build succeeds** → version is created on disk (`builds/vN/binary`,
   `env/vN.enc`) and recorded in `versions.json` with `is_current: false`.
2. **Deploy succeeds** (start/health check passes) → `PromoteVersion` flips
   `is_current: true` and updates the `current` symlink.
3. **Deploy fails** → the version exists on disk for inspection or retry, but
   `is_current` stays `false` and the `current` symlink is never moved. The
   active running instance is untouched.

This means you can always inspect a failed build's artifacts, but a broken
deploy can never corrupt the "current" pointer. See
[state management](../architecture/state-management.md) for the full on-disk
model.

## `phelix rollback <AppName>`

Reverts to a previous versioned build. The rollback path depends on how the app
was deployed:

- **Zero-downtime apps** (built with `--blue-green` or `--replicas`): rollback
  goes through the same deploy path — health check, proxy switch, graceful
  shutdown — so you get the same zero-downtime guarantee.
- **Classic apps** (built with plain `phelix build` / `phelix rebuild`):
  rollback stops the current instance, copies the versioned binary into place,
  and starts it. This is a brief-downtime rollback (stop → start).

```bash
phelix rollback myapp              # interactive picker (TTY), else previous version
phelix rollback myapp --to v3
phelix rollback myapp --to 3       # version number without 'v' prefix also works
phelix rollback myapp --to hotfix-auth   # roll back by tag name
phelix rollback myapp --list
phelix rollback myapp --to v3 --dry-run   # preview only — no changes
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--to` | — | Target: `v3`, `3`, or a tag name (bypasses the interactive picker) |
| `--list` | `false` | List all retained versions with metadata (non-interactive) |
| `--dry-run` | `false` | Preview the rollback plan without changing application, process, proxy, or deployment state |
| `--reason` | — | Record why the rollback was performed (stored in rollback history; max 500 characters) |
| `--verify` | — | Observe rollback stability for a Go duration (e.g. `30s`, `1m`, `2m30s`) after the rollback completes |

The `--to` flag accepts either a version ID (`v3`, `3`) or a unique tag name. If
a tag matches exactly one version, it resolves automatically. If a tag matches
zero or more than one version, an error is returned — use a version ID to
disambiguate.

## Interactive picker

In a TTY, `phelix rollback <App>` without `--to` opens an interactive picker
instead of silently choosing the previous version. It lists valid rollback
targets newest → oldest (current version and versions whose binary is missing
are excluded), shows built-time metadata per entry, and after you press Enter
displays the rollback preview and asks for confirmation before executing.

```text
Rollback 'myapp'

Current: v12

Select version to rollback to:

❯ v11   hotfix-auth           2 min ago
  v10   release-2.4.0         1 hour ago
  v9    stable                yesterday
  v8    —                     3 days ago

↑/↓ select   Enter continue   Esc cancel
```

Moving the cursor updates a details footer for the focused version, rendered
from stored build metadata only (no process is started, no network check runs
while navigating; values that are not stored show `—`):

```text
v11
├── Tag: hotfix-auth
├── Commit: 8f31c2a
├── Built: 2 min ago
├── Binary: 14.8 MB
├── Health: —
└── Deploy: blue-green
```

Pressing Enter shows the same rollback preview as `--dry-run` followed by a
`Proceed with rollback?` confirmation before anything executes.

`Esc` (or `Ctrl-C`) cancels cleanly — `Rollback cancelled.` is printed, nothing
is deployed, and this is not reported as a rollback failure. Without a TTY (CI,
scripts, redirected stdin) the picker never opens and rollback falls back to the
previous-version default, so automation never hangs waiting for input.

**Explicit mode:** `--to v7` resolves and executes the requested target exactly
— no picker, no confirmation prompt — keeping scripts and automation
deterministic. Prefer the picker for interactive inspection and an explicit
`--to` for deterministic automation.

## Rollback reason (`--reason`)

The reason becomes part of the rollback history/audit record, so `phelix
rollback history` explains *why* each rollback happened:

```bash
phelix rollback myapp --to v7 --reason "Login endpoint returning 500"
```

The reason is optional; `--dry-run` displays it in the plan but persists
nothing. Explicitly supplied-but-empty reasons are rejected (`rollback reason
cannot be empty`), whitespace is collapsed, and the text is capped at 500
characters. It is serialized through `encoding/json` — never concatenated — so
it cannot corrupt the structured history or inject log lines. In the interactive
picker the reason is requested after the target version is chosen (Enter skips
it); an explicit `--reason` never re-prompts.

## Rollback verification (`--verify`)

A rollback should not count as successful merely because the process started.
With `--verify <duration>`, Phelix observes the application for the requested
period **after** the rollback reaches its committed, traffic-serving state
(target loaded, environment restored, instance healthy, proxy switched,
deployment state persisted) and fails if it does not remain healthy:

```bash
phelix rollback myapp --to v7 --verify 30s
```

```text
→ Verifying rollback stability...
  5s    ✓ healthy
  10s   ✓ healthy
  ...
✓ Rollback remained healthy for 30s
```

Semantics:

* Verification observes the instances **actually serving traffic** — the active
  blue-green slot, all rolling replicas, or the classic process — never a
  drained slot.
* It reuses the existing tiered health checks and the per-app health
  configuration (`phelix health set`); the duration is the observation window,
  not a request timeout, and it is parsed as a real Go duration (`30` is
  rejected — use `30s`).
* **Verification failure is distinct from rollback execution failure.** The CLI
  prints `Rollback execution: SUCCESS / Verification: FAILED` and exits **23**
  (`ROLLBACK_VERIFY_FAILED`), while an execution failure exits **22**
  (`ROLLBACK_FAILED`). History records the execution as `SUCCESS` with a
  verification block (`passed` / `failed` / `cancelled`).
* `Ctrl+C` during the window cancels the observation (history: `cancelled`); the
  completed rollback stays active. Phelix never rolls forward/backward on its own
  — recovery is your call.
* Omitting `--verify` preserves the existing rollback behavior exactly; no extra
  delay is added.
* `--dry-run --verify 30s` shows the planned window and performs nothing.

Combined:

```bash
phelix rollback myapp \
  --to v7 \
  --reason "Login endpoint returning 500" \
  --verify 30s
```

## Automatic rollback on failed deployment (`phelix rebuild --auto-rollback`)

Add `--auto-rollback` to `phelix rebuild` and a deploy-phase failure restores
the previous known-good version automatically — the same path a manual rollback
takes, so locks, health checks, proxy switching, state reconciliation and
history are shared, not duplicated:

```bash
phelix rebuild myapp --blue-green --auto-rollback
phelix rebuild myapp --replicas 4 --auto-rollback
```

```text
✗ v13 failed health checks
→ Automatic rollback enabled
  → automatic rollback: restoring v12
  ✓ v12 started, healthy and serving traffic
✓ Deployment rolled back automatically
```

Behavior:

* **Failure boundaries.** Build/compile failures never trigger a rollback (no
  new version was recorded, nothing was displaced). Deployment-phase failures
  do: instance start failure, failed health checks, proxy switch failure, and a
  partially-completed rolling rollout.
* **Canary/progressive.** A regression during a rollout is handled by the
  rollout itself: the stable version is restored to 100% of traffic before the
  command returns (`CANARY_REGRESSION`), so there is normally nothing left for
  `--auto-rollback` to do — it reports "kept serving" instead of faking a
  rollback. It still covers the corner cases where the rollout's own restore
  fails.
* **Blue-green.** A failed candidate is killed *before* the traffic switch, so
  the previous version usually kept serving the whole time — Phelix reports that
  ("kept serving") instead of faking a rollback. No history entry is written for
  a rollback that did not happen.
* **Rolling.** If the rollout failed after some replicas switched to the new
  version, recovery redeploys the known-good version over the fleet one replica
  at a time, preserving availability. If it failed at the first replica, the old
  fleet is still intact and nothing is redeployed.
* **Classic.** The known-good binary is copied back and restarted (brief
  downtime is inherent to classic — Phelix does not claim zero downtime).
* **Last known good is authoritative.** The restore target is the version
  `versions.json` currently promotes (a failed deploy never promotes itself),
  restricted to versions whose binary still exists — never "current - 1". With
  no known-good version, Phelix says so instead of fabricating a target.
* **History.** Automatic recoveries appear in `phelix rollback history` with the
  `SOURCE` column set to `automatic` and a reason derived from the deployment
  failure ("Deployment v13 failed: …"). Manual rollbacks show `manual`.
* **Recovery can fail too.** If the previous version cannot be restored safely,
  Phelix prints the degraded state explicitly and exits **24**
  (`AUTO_ROLLBACK_FAILED`) — it never claims success. A recovered deployment
  still exits **21** (the deployment itself failed; the output and history say
  recovery succeeded).
* The rollback is triggered exactly once per failed deployment and cannot
  recurse: recovery failures are terminal, never new rollback triggers.

## Preview with `--dry-run`

Preview exactly what a rollback **would** do — target resolution, strategy,
traffic transition, health checks, and the step-by-step plan — without making
any changes. Nothing is started, stopped, switched, promoted, or written: no
process, proxy, `versions.json`, `deploy.json`, `current` symlink, or
`rollback.log` mutation, and no rollback audit entry is recorded. It is safe to
run any number of times, including while the app serves traffic.

The preview is a **plan and validation preview**, not a guarantee: it reads the
same metadata the real rollback resolves (current/target version, deploy mode,
env snapshot presence, health-tier configuration), validates that the target
artifact exists, and reports warnings — but it does not start instances, perform
live health checks, or predict ports that are only assigned at startup.

```bash
phelix rollback myapp --to v7 --dry-run
```
```text
→ Rollback Preview

  Application    myapp
  Current        v12
  Target         v7
  Built          2026-08-31 14:22:10
  Commit         8f31c2a
  Deploy Mode    blue-green
  Health Check   Tier 1 (/health, 2xx required)
  Environment    v7 snapshot available

Changes:
  Version        v12 → v7
  Binary         15.2 MB → 14.8 MB
  Environment    v7 snapshot available

Traffic:
  Public         :3000
  Current        green
  Target         blue

Rollback Plan:
   1. Ensure the proxy daemon is running
   2. Load the v7 artifact (.../builds/v7/binary)
   3. Restore the v7 environment snapshot (env/v7.enc)
   4. Start the new instance on the inactive slot blue (internal port assigned at startup)
   5. Run health checks against the new instance
   6. Switch proxy traffic on public port 3000 from slot green to slot blue
   7. Promote v7 as current (versions.json + current symlink)
   8. Drain and stop the old slot green instance (grace 30s)

✓ No changes will be made.
```

For **classic** apps the preview shows the stop→start plan and an explicit
`Downtime: Expected yes` line; for **rolling** apps it lists one replacement
step per replica, in the order the real rollback replaces them. If validation
fails (missing binary, unknown version, tag ambiguity, target == current), the
preview reports the same structured error a real rollback would return — with
nothing modified. Warnings (e.g. a missing env snapshot, a large rollback
distance, a deploy lock held by another operation) are shown in a `Warnings:`
section without blocking the preview.

## Listing versions — `--list`

Show all retained versions with metadata:

```bash
phelix rollback myapp --list
```

Output table columns:

- **Version** — `vN` label
- **Tag** — optional label (e.g. `hotfix-auth-bug`), if provided via `--tag`
- **Commit** — git commit hash (if available at build time)
- **Built** — build timestamp (RFC 3339)
- **Size** — binary size on disk
- **Current** — whether this version is actively serving traffic
- **Prune soon** — whether this version would be removed after the next build
  (based on retention policy)

## Rollback history — `phelix rollback history`

Shows the recorded rollback outcomes for one application, newest first. Every
rollback attempt that reaches execution is recorded — successes and failures —
at the moment the rollback transaction completes, together with the deployment
mode actually used for that rollback (`classic`, `blue-green`, `rolling`). The
mode is a historical fact: later re-deploys of the app never rewrite old
records. `FROM`/`TO` are the versions of that transition, not the app's current
version.

| Flag | Default | Description |
|------|---------|-------------|
| `--limit` | `20` | Maximum number of entries to show (must be a positive number) |

```bash
phelix rollback history myapp
phelix rollback history myapp --limit 50
```

```text
Rollback History — myapp

TIME                  FROM   TO     STATUS   MODE         REASON
2026-09-06 14:20:31   v12    v7     SUCCESS  blue-green   Login endpoint returning 500
2026-09-06 13:11:02   v13    v12    SUCCESS  rolling      API regression
2026-09-02 09:13:12   v9     v8     FAILED   rolling      Database migration issue
2026-08-28 18:42:09   v8     v6     SUCCESS  classic      —
```

Notes:

* An app with no rollbacks shows `No rollback history found.` — normal state,
  not an error.
* `--dry-run` previews never record history; cancelling the interactive picker
  (`Esc`) never records history either, and is not a failure.
* Malformed legacy lines in the history file are skipped with a short warning;
  they are never silently rewritten.
* `REASON` shows the recorded `--reason`, or `—` for reason-free and pre-reason
  records (old history entries load unchanged and are never rewritten to add an
  empty reason).
* When `--verify` was requested, each record also carries a verification block
  on disk (`"verification": {"requested": true, "duration": "30s", "status":
  "passed"}` with status `passed` / `failed` / `cancelled`). A failed window
  renders as `VERIFY_FAILED` while `STATUS` stays `SUCCESS` — execution outcome
  and verification outcome are kept separate so history always tells the truth
  about what happened.
* History is stored per app as JSON Lines at
  `~/.phelix/apps/<AppName>/rollback_history.jsonl`.

## How rollback works

Rollback is split into two phases: **resolve + plan**, then **execute**. The CLI
first resolves the target version and deployment strategy and validates the
target artifact (shared by both the preview and the real path); a real rollback
then executes the plan, `--dry-run` renders it and exits.

**Zero-downtime path** (blue-green / rolling apps):
1. `ResolveVersionOrTag` resolves the `--to` argument to a concrete version ID
2. `ExistingVersionSource` resolves the binary and env paths for the target version
3. A new instance starts on the inactive slot (blue or green)
4. Tiered health checks verify the instance is healthy
5. The proxy atomically switches traffic to the new instance
6. The old instance is gracefully drained and stopped
7. The `current` symlink and `versions.json` are updated

If the rollback target fails its health check, the rollback **aborts** and the
active instance is left untouched — identical to a failed forward deploy.

**Classic path** (apps built without `--blue-green` / `--replicas`):
1. `ResolveVersionOrTag` resolves the target version
2. The current instance is stopped
3. The versioned binary is copied to the app's expected location
4. The app is started via `app.Manager.StartApplication`
5. The version is promoted (`is_current` set to true, `current` symlink updated)

If the start fails, the version exists on disk but `is_current` stays false —
you can retry without a broken "current" pointer.

## Rollback safety

- **Dry-run preview**: `--dry-run` shows the full plan with zero mutations —
  verify before you commit.
- **Concurrent protection**: a deploy lock prevents rollback from racing with
  another deploy or rollback on the same app.
- **Versioned env**: binary and env are paired per version; rollback always
  restores both.
- **Audit log**: every rollback attempt (success or failure) is recorded in
  `~/.phelix/apps/<AppName>/rollback.log`.

## Retention policy

Old versions are automatically pruned after each successful build, keeping the
last **5** versions by default. The currently active version is never pruned,
even if it falls outside the retention window. The retention count is
configurable per plan tier (Free: 3, Pro: 10, Enterprise: unlimited).

## Related

- [Command reference](../reference/commands.md#versioning--rollback).
- [Zero-downtime deployments](zero-downtime-deployments.md) — the deploy path
  rollback reuses.
- [State management](../architecture/state-management.md) — `versions.json`,
  `deploy.json`, two-phase promotion internals.
- [Exit codes](../reference/exit-codes.md) — rollback exit codes 22/23/24.
