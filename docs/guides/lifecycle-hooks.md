# Deployment Lifecycle Hooks

Lifecycle hooks run your own commands at precise points of a build, deploy,
rollback or stop. Use them for database migrations, cache warm-up, smoke tests,
notifications and connection draining without wrapping Phelix in a script.

Hooks are optional. A project without a `hooks` section behaves exactly as
before.

## Configuration

Add a `hooks` block to `phelix.yaml`:

```yaml
hooks:
  pre_build:
    command: ["./scripts/check-migrations"]
  post_build:
    command: ["./scripts/verify-binary"]
  pre_deploy:
    command: ["./bin/migrate", "up"]
    timeout: 5m
    retries: 2
    retry_interval: 5s
  post_start:
    command: ["./scripts/smoke-test"]
    timeout: 30s
  pre_traffic:
    command: ["./scripts/warm-cache"]
  post_deploy:
    command: ["./scripts/notify", "success"]
    continue_on_error: true
  pre_stop:
    command: ["./scripts/drain"]
    timeout: 20s
  post_rollback:
    command: ["./scripts/notify", "rollback"]
```

Every hook accepts the same fields:

| Field | Type | Default | Meaning |
|-------|------|---------|---------|
| `command` | list of strings | required | Executable and arguments. Never a shell string. |
| `timeout` | duration | `60s` | Limit for one attempt. Maximum `30m`. |
| `retries` | integer | `0` | Additional attempts after a failure. Maximum `10`. |
| `retry_interval` | duration | `2s` | Delay before the first retry. Later retries double it, bounded by `timeout`. `0` retries immediately. |
| `continue_on_error` | boolean | depends on the hook | Keep the operation running when the hook fails. |
| `working_dir` | path | project root | Relative to the project directory. Must stay inside it. |
| `env` | mapping | none | Extra variables for this hook. |
| `enabled` | boolean | `true` | Turn a declared hook off without deleting it. |

`command` is an argument array, not a shell command. No shell metacharacter in
an argument is interpreted. Run `["sh", "-c", "..."]` explicitly when you need
a shell.

## Execution order

| Hook | Runs | Failure policy |
|------|------|----------------|
| `pre_build` | Before the artifact is built, once per deployment. | Blocks the deployment. |
| `post_build` | After the artifact is built and before deployment starts. | Blocks the deployment. |
| `pre_deploy` | Before any instance or traffic change. | Blocks the deployment. |
| `post_start` | After the new version starts and passes its health check. | Blocks and triggers the normal rollback path. |
| `pre_traffic` | After the new version is healthy and before traffic moves to it. | Blocks and triggers the normal rollback path. |
| `post_deploy` | After the deployment is committed. | Reported, never undoes the deployment. |
| `pre_stop` | Before existing instances are stopped, during deploy or `phelix stop`. | Blocks the stop during a deployment; during `phelix stop` it follows its configured policy. |
| `post_rollback` | After a successful manual or automatic rollback. | Reported, never marks the rollback failed. |

Classic deployments have no separate traffic activation step, so `pre_traffic`
is skipped there. Blue-green, rolling, canary and progressive deployments run
every hook that applies to their traffic switch.

`post_deploy` and `post_rollback` cannot be made critical: they run after the
outcome is already committed, so a failure is recorded but cannot reverse it.
`pre_stop` defaults to non-critical for the same reason during cleanup.

## Hook environment

Every hook receives the normal process environment plus:

| Variable | Value |
|----------|-------|
| `PHELIX_APP` | Application name |
| `PHELIX_APP_ID` | Application ID |
| `PHELIX_PROJECT_DIR` | Absolute project directory |
| `PHELIX_DEPLOYMENT_ID` | Deployment identifier |
| `PHELIX_STRATEGY` | `classic`, `blue-green`, `rolling`, `canary` or `progressive` |
| `PHELIX_VERSION` | Version being deployed |
| `PHELIX_PREVIOUS_VERSION` | Version that was active before the deployment |
| `PHELIX_HOOK` | Hook name |
| `PHELIX_ATTEMPT` | Attempt number, starting at 1 |

Names beginning with `PHELIX_` are reserved and rejected in `env`. Values from
the hook `env` block are redacted from captured output, as are resolved secrets.

## Output and history

Each attempt is stored under `~/.phelix/apps/<app>/hooks/executions/` as an
immutable JSON record. Records hold the command, working directory, timing,
exit code, failure reason and captured stdout/stderr. Each stream is capped at
64 KiB; the record is marked truncated when output was dropped.

History is pruned to the newest 200 executions per application. Override this
with `PHELIX_HOOKS_RETENTION`.

A hook that already succeeded for a deployment is never executed twice, so a
retried command cannot repeat a migration that already ran.

## Inspecting hooks

```bash
phelix hooks list                 # hooks configured for the current project app
phelix hooks list my-app --json
phelix hooks status my-app        # latest status per hook
phelix hooks history my-app       # newest executions first
phelix hooks history my-app --hook pre_deploy --status failed --limit 10
phelix hooks inspect <execution-id> my-app
phelix hooks inspect --last my-app
```

`phelix doctor` validates the hook block. `phelix init --hooks-example` appends
a commented example to a new project.

## Recovery and idempotency

A hook must be safe to run twice. Phelix cannot prove what happened if the
process dies while a hook is running, so on the next operation any attempt left
`pending` or `running` for that deployment is marked `uncertain` and is never
retried automatically. Inspect it with `phelix hooks history` and decide whether
to run the step manually.

Successful attempts are skipped, but a failed attempt is retried according to
`retries`. Make migrations and other side effects idempotent rather than relying
on the retry policy.

## Safe migrations

Use an expand and contract sequence so old and new versions can run together
during a zero-downtime deployment:

1. Expand: add nullable columns, tables or indexes in a `pre_deploy` hook. Keep
   the change backward compatible.
2. Deploy: the new version writes both the old and new shape when needed.
3. Contract: remove the old columns or behavior in a later release, after no
   old version can receive traffic.

Never drop or rename a column in the same deployment that stops using it. A
rollback restores the previous binary, not the previous database schema.

Keep migrations in a dedicated executable and give the hook a realistic
`timeout`. A migration that outlives the timeout is killed with its process
group, so avoid long-running DDL inside a hook.

## Failure behavior

A critical hook failure stops the operation and returns an error with the hook
name. During a zero-downtime deployment the normal rollback path runs, including
`post_rollback`. A `continue_on_error` hook is recorded as failed and the
operation continues.

Hook failures never hide the underlying exit code or stderr: use
`phelix hooks inspect` to see the captured output.
