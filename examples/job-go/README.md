# job-go

The smallest runnable `type: job` example. A job is a one-shot command: Phelix
builds it, runs it on a cron schedule, records every execution, and never
supervises it as a service.

Standard library only, no external dependencies.

## What it demonstrates

- `type: job`, `schedule` and `job` in
  [`phelix.yaml`](../../docs/reference/configuration.md): no `port`, no
  `health`, no `process`, no `deploy` block.
- A cron schedule evaluated in `schedule.timezone`, with `missed: skip`.
- A timeout and one retry, plus `concurrency.policy: forbid` so two runs of
  the same job never overlap.
- Execution history and logs under `~/.phelix/jobs/nightly-report/`.

The program appends one timestamped line to `job-runs.log` in the project
directory and prints it. `-fail` exits 1 and shows up as `failed` in the
history.

## Run it directly (no Phelix)

```bash
cd examples/job-go
go run . -message "manual run"
cat job-runs.log
go run . -fail                 # exits 1
```

## Build and run with Phelix

From this directory (`phelix.yaml` supplies the name, type and schedule):

```bash
phelix build nightly-report    # builds a version; starts no service and no worker
phelix job list                # schedule and command of every job
phelix job run nightly-report  # run once now, regardless of the schedule
phelix job history nightly-report --limit 5
phelix job logs nightly-report # output of the newest execution
phelix job logs nightly-report --execution <ID>
```

The monitor daemon runs the schedule:

```bash
phelix monitor
```

Each run writes one record under `~/.phelix/jobs/nightly-report/executions/`
and a redacted log under `~/.phelix/jobs/nightly-report/logs/`. The example
schedule fires every five minutes, so `phelix job run` is the quickest way to
see a real execution.

## Command resolution

`job.command` runs from the project directory without a shell. A relative
path such as `./nightly-report` is used when it exists in the project; after
`phelix build` it falls back to the current built version binary, which is why
the same command works before and after a build.

## Failure and retries

Run with the failure flag to see retry behavior:

```bash
phelix job run nightly-report
# change the command in phelix.yaml to add "-fail", rebuild, then run again
```

With `retries: 1` the recorded execution has two attempts, keeps the full
output of both, and ends as `failed` with a non-zero exit code.
