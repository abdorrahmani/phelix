# Jobs

A job is a one-shot command run on a cron schedule. It is not a service and not
a supervised process: Phelix builds it as a version, runs the command, records
the result, and waits for the next occurrence. The exit status is the result.

```yaml
name: nightly-report
type: job

schedule:
  cron: "0 3 * * *"
  timezone: Europe/Paris
  missed: skip

job:
  command: ["./nightly-report", "-message", "nightly report"]
  timeout: 2m
  retries: 1
  concurrency:
    policy: forbid
```

A job must declare both `schedule` and `job`. A job must not declare `port`,
`health`, `process` or `deploy`; services and workers must not declare
`schedule` or `job`. `phelix config lint` enforces the same rules as a build.

## Schedule

`schedule.cron` accepts standard five-field cron (`minute hour day month
weekday`) and the `@hourly`, `@daily`, `@weekly`, `@monthly` and `@yearly`
descriptors. The expression is evaluated in `schedule.timezone`, an IANA name
such as `Europe/Paris` or `America/New_York`; the default is `UTC`. Do not embed
`TZ=` or `CRON_TZ=` in the expression.

`schedule.missed` controls occurrences that passed while the monitor was down.
Only `skip` is supported: they are never replayed. Restarting the monitor does
not start a catch-up burst. A job that runs longer than its interval also skips
the occurrences it missed instead of stacking them.

The monitor polls every second and keeps one in-memory next occurrence per job.
Before starting an occurrence it checks persisted records, so the same
scheduled instant is never run twice across a monitor restart.

## Execution

`job.command` is an argv run without a shell, from the project directory.
Piping, globbing and shell builtins are not available; put them in a script or
name the interpreter explicitly, for example
`command: ["/bin/sh", "-c", "..."]`.

A relative executable such as `./nightly-report` is used when the file exists
in the project. If it does not, the current built version binary is used, so the
same configuration works before and after `phelix build`.

Each run receives:

- the monitor process environment;
- the environment snapshot recorded with the version, or the app's live
  encrypted environment before the first build;
- the app's committed secret bindings, merged with the existing
  `secrets` mechanism.

Output is captured per run, line by line, with secrets redacted. Nothing is
written into the app's service log.

`job.timeout` bounds one attempt, including any child process. On expiry the
whole process group receives a kill; the status is `timed_out`.
`job.retries` adds sequential attempts after a failed attempt. Attempts never
run in parallel, and the log keeps the output of every attempt.

`job.concurrency.policy` is enforced across processes:

- `forbid` (default): an execution lock per app. A scheduled occurrence that
  finds the lock held is recorded as `skipped`; a manual `phelix job run`
  returns the lock error.
- `allow`: overlapping runs are permitted.

Execution statuses:

| Status | Meaning |
|--------|---------|
| `pending` | Record written, process not started yet |
| `running` | Process is running |
| `succeeded` | Exit code 0 |
| `failed` | Non-zero exit or start failure |
| `timed_out` | The attempt hit `job.timeout` |
| `cancelled` | The monitor shut down while the run was active |
| `skipped` | An occurrence was deliberately not started |
| `interrupted` | A previous process disappeared while a run was active |

## Build and rebuild

`phelix build <NAME>` and `phelix rebuild <NAME>` build the job like any other
app, record and promote a version, and start nothing. The version mode is
`job`, the app stays `stopped`, and no port, proxy route, health endpoint or
replica is created. `rebuild` rejects `--blue-green`, `--canary` and
`--replicas` for jobs.

`phelix start`, `phelix stop` and `phelix restart` reject job apps. A job is not
a supervised process; use `phelix job run` to trigger one manually.

## Commands

```bash
phelix job list
phelix job run nightly-report
phelix job history nightly-report --limit 5
phelix job logs nightly-report
phelix job logs nightly-report --execution job_a1b2c3d4e5f60718
```

`job run` runs once, now, outside the schedule, and applies the configured
timeout, retries and concurrency policy. It exits non-zero when the recorded
status is not `succeeded`.

`job history` prints one line per execution, newest first. `job logs` prints the
recorded output of the selected execution, or of the newest one when
`--execution` is omitted.

## Persistence

```text
~/.phelix/jobs/<AppName>/
├── executions/<id>.json   # one record per run
├── logs/<id>.log           # redacted output per run
└── locks/                  # per-app run lock
```

Records are written atomically. Corrupt records are skipped when history is
read, so one damaged file never hides the rest. Retention keeps the 200 most
recent finished executions and never deletes an active one.

At startup the monitor closes out `running` and `pending` records left by a
previous process as `interrupted`, unless the recorded process identity still
matches: a manual run may outlive a monitor restart. These semantics are in
[Where Phelix stores data](../reference/data-directory.md).

## Monitor

Only `phelix monitor` runs the schedule. `phelix job run` works without the
monitor. The scheduler starts with the daemon, uses its own context, stops
gracefully on shutdown, and marks an active run `cancelled` when its context is
cancelled.

The scheduler runs due jobs one at a time, so a slow job delays later work
instead of starting it concurrently. If several jobs must fire at the same
instant and one is slow, schedule them a few minutes apart.

## Limitations

- One schedule and one command per app.
- Only `skip` exists for missed occurrences; there is no catch-up or backfill.
- Timers live in the monitor. Without `phelix monitor`, nothing runs on
  schedule, although `phelix job run` still works.
- Host commands only: `deploy.runtime: docker` is rejected.
- Filesystem locks assume one host; there is no distributed scheduler.
- The retained 200-execution window is fixed.

## Example

[`examples/job-go`](../../examples/job-go/) is a runnable Go job that appends a
timestamped line, supports a deliberate failure, and walks through build, run,
history and logs.
