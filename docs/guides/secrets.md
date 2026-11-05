# Secrets

Phelix keeps application credentials in a dedicated encrypted store that is
separate from environment variables, from the configuration file, and from
build versions. A secret is created once, referenced by name in `phelix.yaml`,
and resolved when the application starts or restarts. Values are never shown,
never logged, and never baked into a version.

## The model

- Each application owns one store file under the Phelix data directory
  (`~/.phelix/secrets/<app id>.json` by default, relocatable with
  `PHELIX_DATA_DIR`). The directory is `0700`, the store file and its
  companion lock file are `0600`.
- Values are encrypted with AES-256-GCM under the same master key Phelix
  already uses for environment variables (`~/.phelix/master.key`, `0600`).
- Every write creates a new version. The five most recent versions are kept,
  each identified by a fingerprint, a short HMAC under a key derived from the
  master key. The fingerprint answers "did the value change"; it is not a
  proof of authenticity and it cannot be checked offline without the master
  key.
- `phelix.yaml` references secrets by name only. The file can never contain a
  value.

```yaml
secrets:
  imports:
    - secret: database_url
      env: DATABASE_URL
    - secret: redis_password
      env: REDIS_PASSWORD
```

Each import maps one secret in the application's store onto one environment
variable name the application receives. A secret may feed several variable
names; a variable name may be declared once. Reserved names such as `PORT`
are rejected.

## Commands

| Command | What it does |
|---|---|
| `phelix secrets list` | Name, version, fingerprint, bindings, last apply. Never decrypts. |
| `phelix secrets set NAME` | Store a new version; value from stdin or `--from-file`. |
| `phelix secrets get NAME` | Metadata only: version, fingerprint, timestamps, bindings. There is no way to print a value. |
| `phelix secrets history NAME` | Retained versions with fingerprints. |
| `phelix secrets remove NAME` | Delete the secret and all versions. Refused while an import still references it. |
| `phelix secrets apply` | Make the imports in `phelix.yaml` the active bindings, then restart the application if it is running. |

All commands accept an explicit application argument
(`phelix secrets list MyApp`) or resolve the application from
`phelix.yaml` in the current directory. Every command also accepts `--json`
for a machine-readable envelope.

### Providing a value

A value is read from stdin or from a file, never from a command argument,
because arguments are visible to every process on the host:

```sh
phelix secrets set database_url < db-url.txt
printf '%s' "$VALUE" | phelix secrets set database_url
phelix secrets set api_key --from-file key.txt
```

Standard input keeps the value byte for byte except for exactly one trailing
newline, so an interactive shell history entry does not become part of the
secret. `--from-file` stores the file byte for byte. Multi-line values such
as PEM keys are fully supported. Empty values and values containing NUL
bytes are rejected, and a value may be at most 1 MiB.

### Applying a change without a rebuild

`phelix secrets set NAME` stores the new version and, when the secret is
imported by a running application, restarts that application through the
same path `phelix restart` uses. The restart honors the application's
deployment strategy. Use `--no-apply` to store the value without touching
the running application; it applies at the next start, or when you run
`phelix secrets apply`.

If the restart fails, the command says so and exits non-zero: the new value
remains stored (the desired state) while the running application keeps the
previous values (the runtime state). `phelix secrets list` shows the outcome
of the last apply under "last apply", and a retry is one `phelix restart`
away.

## Secrets are independent of versions

Plain environment variables are snapshotted with every build, and a rollback
restores the snapshot of the target version. Secrets deliberately do not
participate in that mechanism:

- Secret values never enter the environment store, so they never enter a
  version snapshot. Deleting a secret leaves no copy behind in history.
- Secrets are resolved from the store at every start, restart and rollback.
  Rolling back an application restores its binary and its plain variables,
  while credentials stay at their current values. A rotated or deleted
  credential can never be resurrected by a rollback.

## Deployments and the deployment journal

During a deployment, all instances must receive the same credentials. Phelix
pins every binding and version once at the start of the deployment, and every
instance the deployment launches receives exactly that set, even if the value
changes mid-deployment (which the deploy lock prevents anyway).

The deployment also opens a binding transaction: the candidate bindings are
recorded in the secret store before any instance changes, and the deployment
engine stamps its own journal (`deploy.json`) with the transaction's
identifier on every durable write it accepts. The stamp is the single source
of truth:

- If the journal shows the transaction was accepted (fully or partially),
  the candidate bindings are committed.
- If the journal shows it was never accepted, the candidates are discarded
  and the previous bindings stay active; instances that were launched but
  never recorded are reclaimed by the deployment engine's own recovery.
- If the journal cannot be read, nothing is committed: the transaction stays
  pending and the situation is visible in `phelix doctor`.

The daemon settles any leftover transaction at startup, and
`phelix secrets apply` settles it on demand. A rollback never reverts
credentials: promoted bindings stay active.

## What appears in output

No command in this feature prints a secret value, and there is no flag that
would. `phelix secrets get` is metadata-only by construction, not by
filtering. On top of that, every value a Phelix process resolves is masked
in everything that process renders or transmits: log records (file and
long-term store), the stream to the dashboard, CLI rendering, error output,
and docker command lines. The masking is reference-counted and uncapped; a
value stays masked as long as a running instance or a draining log stream
could still produce it.

Phelix redacts what passes through Phelix. If an application writes its own
secret to a destination Phelix does not manage, such as journald, its own
file, or a third-party service, that output is outside this guarantee.

## Failure handling

- A declared import with no value in the store stops the deploy, the apply,
  or the start with `SECRET_NOT_FOUND`. Nothing starts partially configured.
  Create the value with `phelix secrets set` or remove the import.
- An import whose variable name is already set as a plain environment
  variable stops the deploy or apply with a clear error. Resolve it with
  `phelix env unset MyApp NAME`, or remove the import. Historical snapshots
  that still carry the same name are handled silently: the managed value
  replaces it, exactly once.
- If one application's store is unreadable or a value cannot be decrypted,
  only that application is affected: its launches stop and its log streams
  are withheld until it resolves again, while other applications continue.
  `phelix doctor` reports the application and the reason.

## Storage layout

```
~/.phelix/secrets/
├── <app id>.json     secrets (encrypted, last 5 versions), active bindings,
│                     pending transaction, last apply
└── <app id>.lock     lock file; created once, never replaced or deleted
```

Every read takes a shared lock and every write an exclusive lock on the lock
file, so concurrent commands serialize instead of losing updates. The store
file itself is replaced atomically on every save.

## Related

- [Environment variables](environment-variables.md): plain variables, which
  remain the right tool for non-credential configuration.
- [Security](security.md): the encryption-at-rest summary and the full
  output-redaction picture.
- [Zero-downtime deployments](zero-downtime-deployments.md): the restart
  paths a secret change reuses.
