# Identity Model

Because Phelix runs on the host (not inside the app containers), the host is the
one agent, and its identity is stable across everything Docker does to the app
containers. The identity model is:

```text
machine_id ≠ agent_id ≠ app_id ≠ instance_id
```

- `agent_id` identifies the one persistent Phelix runtime on this host. Created
  once at `~/.phelix/agent-id` (or `/var/lib/phelix/agent-id`) and reused on every
  start, so restarting the daemon, rebuilding an app, or recreating a container
  never creates a second server on the backend.
- `app_id` identifies an application independently of its agent, persisted in
  `apps.json`.
- `instance_id` identifies a particular running instance — for the Docker runtime,
  a container.
- `machine_id` is never used as the agent identity.

Relocate all of this state with `PHELIX_DATA_DIR` if you don't want the default
location; the app images Phelix builds and runs are ordinary Docker images and
carry none of this identity.

## Application identity

`phelix build billing` (and equivalent application-creation flows) generates an
`app_id` once and persists it in `apps.json`. Its ID is independent of the agent,
the server, and the app name, so renaming or redeploying an app never changes its
identity. App names are unique within one agent, not globally.

## Why one host is one server

This model is why the [Docker runtime](../guides/docker-runtime.md) runs a single
Phelix agent on the host that manages many app containers, rather than a Phelix
inside each container: one `agent_id` = one server on the dashboard, no matter how
many app containers the host runs. Installing a separate Phelix inside each
container would register each as its own server sharing one physical host — the
exact problem this model avoids.

## Related

- [Docker runtime](../guides/docker-runtime.md) — the runtime that relies on this
  model.
- [State management](state-management.md) — where `app_id` and instance state live
  on disk.
- [Monitoring](monitoring.md) — how the agent identifies itself to the backend.
