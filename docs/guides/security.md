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

## Secrets at rest

Per-app environment variables and registry credentials are encrypted with
**AES-256-GCM** under `~/.phelix/master.key` (`0600`, auto-generated). Keys
containing `SECRET`, `KEY`, `TOKEN`, or `PASSWORD` are masked in logs and output.
Each versioned build snapshots the encrypted env so rollback restores matching
secrets. See [Environment variables](environment-variables.md).

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

## Transport security

The agent gRPC channel's transport-security model, certificate renewal, and SPKI
pinning implications are specified in the
[TLS & certificate policy contract](../architecture/backend-contracts/tls-cert-policy.md).
`mode: dev` is the only mode that uses plaintext gRPC (for local backends); every
other mode forces TLS with no insecure fallback — see
[development setup](../development/development-setup.md).

## Related

- [Authentication](authentication.md), [Environment variables](environment-variables.md),
  [Webhooks](webhooks.md), [Docker runtime](docker-runtime.md).
- Backend contracts:
  [TLS & certificate policy](../architecture/backend-contracts/tls-cert-policy.md),
  [CLI changes required](../architecture/backend-contracts/CLI_CHANGES_REQUIRED.md).
