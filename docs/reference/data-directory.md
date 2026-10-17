# Where Phelix Stores Data

All state lives under `~/.phelix/` (relocatable with `PHELIX_DATA_DIR` — see the
[environment reference](environment.md)):

```text
~/.phelix/
├── apps.json              # registry of all managed apps
├── master.key              # AES-256-GCM master key for env encryption (0600)
├── session.json            # auth session
├── config.json             # server configuration
├── proxy.sock               # proxy daemon control socket
├── logs/
│   ├── phelix.log           # Phelix's own log
│   ├── <app>.log            # per-app logs
│   └── deploy_*.log         # deploy instance logs
├── registry/<slug>.enc      # encrypted registry credentials
├── cache/                    # regenerable caches (safe to delete)
│   └── toolchain-<lang>.json # cached toolchain version (see Toolchain version cache)
├── authz/                    # execution authorization state (see Authorization)
│   ├── policy.json            # host authorization policy (0600; absent = legacy local mode)
│   ├── approvals/             # immutable per-plan approvals (<plan-id>.json, 0600)
│   └── decisions.jsonl        # authorization decision log (bounded to 512 records, 0600)
├── matrix/
│   └── runs/                 # Matrix Run history: <id>.json records,
│                             #   <id>.lock execution locks, <id>.manifest.json
│                             #   release manifests (see Matrix builds)
├── webhook/
│   ├── deliveries.json       # webhook delivery dedup ledger (replay protection;
│   │                         #   bounded to the 512 most recent deliveries)
│   ├── worktrees/            # temporary isolated Git sources (one per webhook
│   │                         #   job, removed after the deploy; swept at startup)
│   └── jobs/                 # durable deployment-job records (wh_<id>.json;
│                             #   bounded to the 200 most recent finished jobs)
├── sessions/                 # agent execution sessions (see Agent Sessions)
│   ├── <session-id>.json      # one durable tracking record (ses_<16 hex>.json, 0600)
│   └── <session-id>.lock      # per-session advisory lock serializing updates
└── apps/<AppName>/          # per-app data
    ├── versions.json        # version metadata index (incl. per-version build reports + per-combo matrix reports)
    ├── deploy.json           # blue-green / rolling state
    ├── rollback.log          # rollback audit trail
    ├── current → builds/vN   # symlink to the active build
    ├── builds/vN/binary       # versioned binaries
    └── env/vN.enc              # per-version encrypted env snapshot
```

> The agent-scoped monitor session is stored separately at
> `~/.phelix/agent-session.json` (see [Authentication](../guides/authentication.md)),
> and per-app rollback history at `~/.phelix/apps/<AppName>/rollback_history.jsonl`
> (see [Rollback](../guides/rollback.md)). The proxy persists enrolled apps and
> targets to `~/.phelix/proxy-state.json`
> (see [Zero-downtime deployments](../guides/zero-downtime-deployments.md)).

Everything under `cache/` is derived data. It can be deleted at any time and is
rebuilt on demand, unlike the records above.

## Retention

The last **5** versions are kept by default (configurable per plan); the active
version is never pruned. Build-report metadata lives inside `versions.json` (the
`build_report` field per version) — there is no separate build database, and
everything works offline.

## Toolchain version cache

`cache/toolchain-<lang>.json` holds one small record per language, so a build
reports the compiler version without spawning `go version` or `rustc --version`
on every run:

```json
{"path": "/usr/lib/go/bin/go", "size": 15894528, "mtime_ns": 1791505098000000000, "version": "1.27"}
```

The record is reused only while the toolchain binary it was read from is
unchanged. `path`, `size` and `mtime_ns` all have to match, which covers an
upgrade, a reinstall, and a version manager retargeting a shim such as `asdf`
or `rustup`. A mismatch means the next build probes again and rewrites the
entry, so the cache self-corrects after a toolchain change with no manual step.

It is an optimisation only. Any unreadable, malformed or stale entry is treated
as a miss, and an unwritable cache directory is ignored rather than reported, so
the cache can never fail a build or change its output. Deleting `cache/` is
always safe; the next build recreates it.

## Data directory location

`PHELIX_DATA_DIR` relocates all state. Otherwise Phelix uses `/var/lib/phelix`
inside Docker (detected via `/.dockerenv`), else `~/.phelix`. See the
[environment reference](environment.md).

## Related

- [State management architecture](../architecture/state-management.md) — the
  meaning of and relationship between `apps.json`, `deploy.json`, and
  `versions.json`.
- [Configuration](configuration.md), [Environment](environment.md).
- [Authorization](../guides/authorization.md) — the `authz/` policy, approval
  and decision-log files.
- [Agent Sessions](../guides/agent-sessions.md) — the `sessions/` tracking
  records (distinct from the top-level `session.json` dashboard auth session).
