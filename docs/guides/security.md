# Security

A summary of Phelix's security-relevant behavior, with pointers to the guides
and contracts that cover each area in depth.

## Security considerations

- All communication with the Phelix service is encrypted (only when logged in —
  offline mode sends nothing).
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

`phelix security` inventories the Go and Rust dependencies of an
application and evaluates them against OSV.dev advisories, fully locally:
dependency files are parsed in place, nothing is built, and nothing is
downloaded. Go dependency versions are resolved authoritatively with
`go list -m` (proxy disabled: resolution uses the local module cache only,
so a warm cache from normal builds answers offline); without a usable
toolchain the scan degrades to manifest evidence and says so. The advisory
query sends ecosystem and package names to OSV.dev; dependency data
reaches the Phelix backend only after you opt in (below).

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
phelix security scan [app]      # scan; progress renders on stderr, --json stays pure
phelix security report [app]    # latest completed scan, --previous for the diff
phelix security history [app]   # recorded scans with status and counts
phelix security show <ID>       # one advisory, cache first
phelix security config [app]    # show; --set-* flags edit phelix.yaml safely
```

Run without arguments in a terminal, the commands guide you: `phelix
security` opens a menu, and `scan`, `report`, `history`, `show`, and
`config` ask for a target application. Scripts and CI keep the exact
non-interactive behavior.

Configuration edits preserve unrelated YAML, comments, and formatting,
validate before writing, and serialize concurrent Phelix edits through a
lock:

```bash
phelix security config myapp --enable --scan-interval 12h --fail-threshold medium
phelix security config myapp --disable
```

Scheduled scans run opt-in through the monitor daemon:

```yaml
security:
  vulnerability:
    enabled: true
    scan_interval: 24h
    fail_threshold: high
```

### Remote access and reporting consent

The dashboard can view reports, dependency graphs, scan history, and
advisory details, and can trigger scans or change settings, over the same
authenticated gRPC channel as every other remote operation. Because
dependency names and versions are private, every application opts in
explicitly:

```yaml
security:
  vulnerability:
    reporting: true
```

With `reporting` disabled (the default), remote security operations
against the application answer `SECURITY_REPORTING_DISABLED` and the
monitor pushes nothing. With it enabled, the monitor pushes a summary of
the latest completed scan on every stream sync, and the full reports,
graphs, and configuration travel only on explicit backend queries. Remote
mutations (running a scan, changing settings) additionally pass through
the host authorization boundary (`security` action) and carry persistent
idempotency, so a retried request never duplicates work.

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

Errors never leak secrets — not even under `--debug`; every rendered string
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

The important distinction: the **encrypted env store is world-readable (`0644`)
by design** — its confidentiality comes from the encryption, not the filesystem
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
host-root — do it only deliberately. See [Docker runtime](docker-runtime.md).

## Execution authorization

Deployment execution can be gated at a single boundary between a validated
plan and the engine: `request → plan → authorization → (approval) →
execution`. It is **off by default** — a host with no
`~/.phelix/authz/policy.json` behaves exactly as before — and fails closed
once configured: a broken policy denies rather than degrading to "allow
everything", and every refusal is a zero-mutation outcome. Authorization
binds to the plan's content hash, so a grant for one plan can never execute
another. See [Authorization](authorization.md).

Note that `phelix auth login` authenticates you to the **dashboard backend**
and grants no execution rights on the host; the two are independent.


## Related

- [Authentication](authentication.md), [Authorization](authorization.md),
  [Environment variables](environment-variables.md),
  [Webhooks](webhooks.md), [Docker runtime](docker-runtime.md).