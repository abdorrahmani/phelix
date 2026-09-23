# Environment Variables

Phelix reads a small set of environment variables. Two categories: variables
**Phelix sets for your app**, and variables **you set to configure Phelix**.

## Set by Phelix for the managed app

| Variable | Meaning |
|----------|---------|
| `PORT` | The internal port the instance must listen on. Phelix assigns it per instance/slot and injects it; the app must bind it. See [The PORT Contract](../getting-started/the-port-contract.md). |

Encrypted per-app env variables (`phelix env set …`) are also injected into the
app process at start — see the
[Environment variables guide](../guides/environment-variables.md).

## Set by you to configure Phelix

| Variable | Effect |
|----------|--------|
| `PHELIX_DATA_DIR` | Relocates all Phelix state. Otherwise `/var/lib/phelix` inside Docker (detected via `/.dockerenv`), else `~/.phelix`. |
| `PHELIX_MODE` | Backend environment/mode override (e.g. `dev`). `dev` uses plaintext gRPC for a local backend; any other value forces TLS with no insecure fallback. |
| `PHELIX_API` | Overrides the backend REST API base URL (e.g. `http://phelix.local/api/v1`). |
| `PHELIX_GRPC_URL` | Overrides the monitor gRPC endpoint (`host:port`, e.g. `phelix.local:50051`). |
| `PHELIX_RUNTIME` | `docker` runs app instances as containers for this invocation (precedence: `PHELIX_RUNTIME` → `phelix.yaml` `deploy.runtime` → `native`). See [Docker runtime](../guides/docker-runtime.md). |
| `PHELIX_CGROUP_ROOT` | Clean absolute path of the delegated parent cgroup for [resource limits](../guides/resource-limits.md); otherwise resolved via `/proc/self/mountinfo` + `/proc/self/cgroup`. |
| `PHELIX_WEBHOOK_SECRET` (or your configured `secret_env`) | The HMAC-SHA256 shared secret the webhook server verifies against. Named indirectly by `webhook.secret_env`; never stored in `phelix.yaml`. See [Webhooks](../guides/webhooks.md). |
| `PHELIX_RELEASE_BASE_URL` | Overrides the release server base URL used by `phelix update` (see [release process](../development/release-process.md)). |

The `PHELIX_MODE` / `PHELIX_API` / `PHELIX_GRPC_URL` overrides are applied
independently and exist so a local build can target a local backend **without
editing the embedded `config/config.yml`**. See
[development setup](../development/development-setup.md).

```sh
PHELIX_MODE=dev \
PHELIX_API=http://phelix.local/api/v1 \
PHELIX_GRPC_URL=phelix.local:50051 \
  phelix build myapp
```

## Related

- [Configuration](configuration.md) — the `phelix.yaml` project file.
- [Data directory](data-directory.md) — where `PHELIX_DATA_DIR` points.
- [Development setup](../development/development-setup.md) — local backend
  overrides in depth.
