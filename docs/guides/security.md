# Security

A summary of Phelix's security behavior, with links to the guides that cover
each area in depth.

## Security considerations

- Production communication with the Phelix service is encrypted. Local
  development mode can use plaintext connections; offline mode sends nothing.
- Authentication tokens are securely stored (`~/.phelix/session.json`).
- Server-to-server communication is authenticated.
- Sessions are regularly validated.
- Secure file permissions on sensitive files (`master.key` at `0600`).
- Environment variables and registry credentials are encrypted at rest with
  AES-256-GCM and never logged in plaintext.
- Webhook shared secrets live only in environment variables (never in
  `phelix.yaml`), signatures are verified with constant-time comparison, and the
  server binds to `127.0.0.1` unless you explicitly pass `--host`.
- When not logged in, no app data, metrics, or events leave your machine.

## Dependency vulnerability scanning

`phelix security scan` inventories the Go and Rust dependencies of an
application and evaluates them against OSV.dev advisories. The command does
not compile the application or edit its dependency files. Go dependency versions
are resolved authoritatively with
`go list -m` (proxy disabled: resolution uses the local module cache only,
so a warm cache from normal builds answers offline); without a usable
toolchain the scan degrades to manifest evidence and says so. The advisory
query sends ecosystem and package names to OSV.dev. Remote reporting requires
the separate server activation described below.

Findings carry their evidence class: a dependency is `affected` when its
resolved version is established (Cargo.lock, a pinned replace directive,
or the resolved Go module graph) and lies inside the affected range;
manifest minimums report `potentially_affected` with the reasoning
attached; anything that cannot be decided reports `unknown` rather than
silently clearing. Reachability is always `unknown`. A scan exits **25**
when findings reach the configured threshold, which makes the command a
usable CI gate; an offline scan that could not check every package reports
`advisory_coverage: partial` and never presents itself as clean.

Commands:

```bash
phelix security scan myapp --json
phelix security report myapp --previous
phelix security history myapp
phelix security show GO-2024-2687
phelix security config myapp
```

Run without arguments, `phelix security` prints its subcommand help, and
in a terminal `scan`, `report`, and `config` ask for a target
application. Scripts and CI keep the exact non-interactive behavior.

Configuration edits preserve unrelated YAML, comments, and formatting,
validate before writing, and serialize concurrent Phelix edits through a
lock:

```bash
phelix security config myapp --set-enabled true --set-scan-interval 12h --set-fail-threshold medium
phelix security config myapp --set-enabled false
```

Scheduled scans run opt-in through the Controller:

```yaml
security:
  vulnerability:
    enabled: true
    scan_interval: 24h
    fail_threshold: high
```

## Versioned artifact security

Native Go and Rust builds now record SPDX 2.3 and CycloneDX 1.6 SBOMs,
resolved dependency relationships, license findings, SLSA provenance v1 and the
artifact's SHA-256. You can rescan a stored version's exact inventory, sign its
artifact and documents with Cosign, and apply audit or enforce policies.
Integrity is recalculated before launch and rollback. See
[Supply chain security](supply-chain-security.md) for commands, trust settings
and the limits of native artifact coverage.

## Remote security activation

Remote security reporting is disabled by default and belongs to the server.
Enable it explicitly using a full account session in the Controller's state
directory:

```bash
phelix auth login
phelix security remote enable
phelix security remote status --json
phelix security remote config
phelix security remote scan myapp
phelix security remote report myapp --json
phelix security remote disable
```

`enable` and `disable` require authentication and applicable host authorization.
An agent-scoped monitoring session cannot perform activation. The `scan`
operation also requires a full account session and applicable application
authorization. `status` and `config` inspect the same persisted activation state.
All six remote subcommands accept `--json`.

Activation is stored at `security/remote.json` inside the Phelix data directory,
normally `~/.phelix`, or the directory selected by `PHELIX_DATA_DIR`. Container
installations default to `/var/lib/phelix`. The state survives restarts and
records a revision, synchronization status and any synchronization error.
`pending` means the change is saved locally but needs a connected Controller;
`synchronized` means the current revision was acknowledged; `failed` includes
the reported synchronization error.

The YAML setting `security.vulnerability.reporting` no longer activates or
disables remote reporting. YAML still controls scheduled local scans. Local
scans and artifact checks continue to work while remote reporting is disabled.

When enabled, security history for registered applications can be synchronized
even if those applications are not watched. This does not turn on their metrics
or log monitoring. The `remote scan` and `remote report` CLI commands operate on
the current Controller's registered application; select another server through
the dashboard. A scan uses the current project's dependencies, while
`supply-chain scan --version` rescans a stored artifact's inventory.

Disabling stops new security uploads and remote scan and report access. Previously
synchronized artifact history and operation audit records remain available to authorized
dashboard users. Local historical reports remain on disk. Remote operations use
stable request IDs: retry an uncertain operation with the same ID and payload.
An interrupted operation can report `SECURITY_OUTCOME_UNKNOWN`; a timeout does
not establish that it failed. Cancellation terminates supported work and reports
its final outcome after the work exits.

## Secrets at rest

Per-app environment variables and registry credentials are encrypted with
**AES-256-GCM** under `~/.phelix/master.key` (`0600`, auto-generated). Keys
containing `SECRET`, `KEY`, `TOKEN`, or `PASSWORD` are masked in logs and output.
Each versioned build snapshots the encrypted env so rollback restores matching
secrets. See [Environment variables](environment-variables.md).

Application credentials that need versioning, rotation and launch-time
resolution live in the dedicated [secret store](secrets.md): per-app, `0700`
directory, `0600` files, referenced from `phelix.yaml` by name only, resolved
at start or restart, and fully decoupled from version snapshots so a rollback
never resurrects an old credential. Every value a Phelix process resolves is
masked in that process's logs, dashboard stream, CLI rendering and error
output for as long as a running instance or a draining log stream could still
produce it.

Errors are redacted even under `--debug`; every rendered string
passes through a redactor, and env errors carry the key *name* only. See
[error codes](../reference/error-codes.md).

## The `~/.phelix/` permission model

Phelix relies on encryption for confidentiality and applies restrictive
permissions to the key and session material specifically. The permissions it
actually sets (verified in the implementation):

| Path | Mode | Notes |
|------|------|-------|
| `~/.phelix/` (created with the master key) | `0700` | directory created when the master key is first written |
| `~/.phelix/master.key` | `0600` | AES-256-GCM master key |
| `~/.phelix/session.json`, `~/.phelix/agent-session.json` | `0600` | auth tokens |
| `~/.phelix/envs/` | `0755` | directory holding per-app encrypted env stores |
| `~/.phelix/envs/<appID>.env.enc` | `0644` | **contents are AES-256-GCM encrypted**; confidentiality does not depend on the file mode |
| `~/.phelix/secrets/` | `0700` | per-app secret stores (see [Secrets](secrets.md)) |
| `~/.phelix/secrets/<appID>.json` | `0600` | encrypted, versioned secret values plus binding metadata; values are never plaintext |
| `~/.phelix/secrets/<appID>.lock` | `0600` | lock file; created once, never replaced, so concurrent commands serialize |
| Docker runtime `--env-file` (decrypted, per launch) | `0600` | decrypted values never appear on the argv or in `docker inspect` |
| Server settings (`ServerInfo` persistence) | `0600` | write-only connection fields are also scrubbed before the wire |
| `security/remote.json` in the Phelix data directory | `0600` | server reporting activation and synchronization state |
| `supply-chain/` in the Phelix data directory | `0700` directories, `0600` files | artifact reports, policies and exact document bytes |

The important distinction: the **encrypted env store is world-readable (`0644`)
by design**; its confidentiality comes from the encryption, not the filesystem
mode, so exposure of the `.enc` file does not expose secrets without
`master.key`. The key, sessions, and decrypted Docker env-files are the artifacts
protected by `0600`. Beyond these specific files, Phelix does not document or
enforce a broader permission policy on every state file under `~/.phelix/`; treat
the directory as sensitive and protect it at the OS level.

## Least-privilege monitoring tokens

Provision monitor-only servers with an [agent-scoped
session](authentication.md#agent-scoped-sessions-for-the-monitor-daemon-recommended)
(`phelix auth login --scope agent`) so a compromised server exposes a
monitoring-only token, not your whole account.

## Webhooks

Push webhooks are HMAC-SHA256 authenticated (constant-time comparison), the
shared secret lives only in an environment variable, and the server binds to
`127.0.0.1` by default. See [Webhooks](webhooks.md).

## Docker runtime

Managed containers publish only to `127.0.0.1` (the proxy is the sole public
entry point), and secrets reach containers via a `0600` `--env-file` rather than
the command line. Mounting the Docker socket into a containerized Phelix grants
host-root privileges. Do it only deliberately. See [Docker runtime](docker-runtime.md).

## Execution authorization

Deployment execution can be gated at a single boundary between a validated
plan and the engine: `request → plan → authorization → (approval) →
execution`. It is **off by default**. A host with no
`~/.phelix/authz/policy.json` behaves as before, and authorization fails closed
once configured: a broken policy denies rather than degrading to "allow
everything", and every refusal is a zero-mutation outcome. Authorization
binds to the plan's content hash, so a grant for one plan can never execute
another. See [Authorization](authorization.md).

Note that `phelix auth login` authenticates you to your **dashboard account**
and grants no execution rights on the host; the two are independent.


## Related

- [Authentication](authentication.md), [Authorization](authorization.md),
  [Supply chain security](supply-chain-security.md),
  [Environment variables](environment-variables.md),
  [Webhooks](webhooks.md), [Docker runtime](docker-runtime.md).
