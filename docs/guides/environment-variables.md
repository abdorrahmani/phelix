# Encrypted Environment Variables

`phelix env` manages per-app secrets encrypted at rest with **AES-256-GCM**. The
encryption key lives at `~/.phelix/master.key` (auto-generated, `0600`
permissions).

```bash
phelix env set    MyApp DATABASE_URL=postgresql://localhost/db
phelix env get    MyApp DATABASE_URL      # sensitive values are masked
phelix env list   MyApp                   # values shown as ***REDACTED***
phelix env unset  MyApp DATABASE_URL
phelix env check  MyApp DATABASE_URL      # exit-status friendly existence check
```

Encrypted values are injected into the app process at start, and snapshotted
alongside each versioned build so a rollback restores the matching env.

Keys containing `SECRET`, `KEY`, `TOKEN`, or `PASSWORD` are automatically masked
in logs and command output.

## Versioned secrets and rollback

Each versioned build snapshots the encrypted env into `env/vN.enc`, paired with
the binary at `builds/vN/binary`. Because the pair is versioned together, a
[rollback](rollback.md) always restores a known-good binary **and** its matching
env — never binary-only. See [state management](../architecture/state-management.md)
for the on-disk layout.

## In the Docker runtime

When running instances as containers, secrets reach the container through a
`0600` `--env-file`, never as `-e` on the command line, so decrypted values
never appear in `docker inspect` or the host process table. See
[Docker runtime](docker-runtime.md).

## Related

- [Security](security.md) — encryption at rest and secret-handling guarantees.
- [Data directory](../reference/data-directory.md) — where `master.key` and the
  per-version env snapshots live.
- [Command reference](../reference/commands.md#encrypted-environment-variables).
