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

## Retention

The last **5** versions are kept by default (configurable per plan); the active
version is never pruned. Build-report metadata lives inside `versions.json` (the
`build_report` field per version) — there is no separate build database, and
everything works offline.

## Data directory location

`PHELIX_DATA_DIR` relocates all state. Otherwise Phelix uses `/var/lib/phelix`
inside Docker (detected via `/.dockerenv`), else `~/.phelix`. See the
[environment reference](environment.md).

## Related

- [State management architecture](../architecture/state-management.md) — the
  meaning of and relationship between `apps.json`, `deploy.json`, and
  `versions.json`.
- [Configuration](configuration.md), [Environment](environment.md).
