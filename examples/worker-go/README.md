# worker-go

The Go counterpart to [simple-go](../simple-go/): the smallest program that
shows what Phelix requires from a **worker** workload. The requirement is the
mirror image of the PORT contract: the app must **not** listen on anything. It
is a long-running background process (a simulated job consumer) that Phelix
supervises, restarts, scales and health-checks.

Standard library only, no external dependencies.

## What it demonstrates

- `type: worker` in [`phelix.yaml`](../../docs/reference/configuration.md): no
  `port`, no `health.endpoints`, no proxy: Phelix starts N replicas and never
  allocates or injects a port.
- The graceful-shutdown contract: the app treats `SIGTERM` as *finish the
  current job, then exit*. Phelix owns the signal and the wait window
  (`process.shutdown.grace_period`); acknowledging or rejecting the work stays
  the app's responsibility (see
  [Workers](../../docs/guides/workers.md)).
- Restart policy, jittered backoff, `crashloop`, and (with the exec probe
  configured) health-driven restarts.

## Run it directly (no Phelix)

```bash
cd examples/worker-go
go run .                            # prints one line per simulated job; Ctrl+C exits cleanly
go run . healthcheck                # exit 0 while /tmp/worker-go-unhealthy is absent
touch /tmp/worker-go-unhealthy      # the healthcheck subcommand now exits 1
```

There is no port to curl: the program prints one line per simulated job and
exits cleanly on SIGTERM after acknowledging the current job.

## Demo knobs (environment variables)

| Variable | Effect |
|----------|--------|
| `CRASH_AFTER_SEC` | exit 1 after this many seconds; demonstrates the restart policy and backoff ladder, ending in `crashloop` |
| `EXIT_CLEAN_AFTER_SEC` | exit 0 after this many seconds; under `on-failure` this is a `completed` replica, not a failure |
| `JOB_INTERVAL_MS` | tick cadence (default `1000`) |
| `GRACE_JOBS_MS` | how long finishing the current job takes on SIGTERM (default `2s`) |

`worker-go healthcheck` is the exec probe: exit 0 unless the marker file
`/tmp/worker-go-unhealthy` exists. Switch `health.mode` to `exec` in
`phelix.yaml` and `touch /tmp/worker-go-unhealthy` to watch a live replica get
restarted by the health check while its process is still alive.

## Build and run with Phelix

From this directory (`phelix.yaml` supplies the name, type and replica count):

```bash
phelix build email-worker          # builds and starts 3 replicas, no port asked
phelix status email-worker         # replica tree: pid, status, restarts, last exit
phelix scale email-worker --replicas 5
phelix log email-worker --replica 2
phelix restart email-worker --replica 2
phelix stop email-worker           # graceful stop of every replica
```

The monitor daemon supervises the replicas between deploys:

```bash
phelix monitor                     # in a terminal or a systemd unit
# kill -9 one replica → it is restarted with backoff, restarts=N in status
```

## Deploy a change

```bash
# edit main.go, then:
phelix rebuild email-worker        # rolling replacement, one replica at a time
```

The old replica keeps running until its replacement passed the startup gate,
then is drained within `deploy.rolling.drain_timeout`.

## Further reading

- [Workers](../../docs/guides/workers.md): supervision, policies, health
  modes, recovery contract.
- [simple-go](../simple-go/): the service-side counterpart (the PORT
  contract).
- [CLI Commands](../../docs/reference/commands.md)
- [Configuration](../../docs/reference/configuration.md)
