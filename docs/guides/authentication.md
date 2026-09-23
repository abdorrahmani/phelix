# Authentication

Authentication is **optional**. Logging in links your local Phelix server to
your account at `phelix.anophel.com` so that app data, resource metrics, and
events are pushed to your dashboard.

You can build, rebuild, start, restart, stop, and manage applications **without
logging in** — everything works locally. When you're not logged in, Phelix notes
during build/rebuild that no metrics or events will be sent to the dashboard
until you authenticate.

Once you log in, monitoring data starts flowing: events from commands you run
and, if the monitor daemon is running, resource metrics and logs. Apps you
created **before** logging in are synced to your dashboard at login time, so
nothing you built while offline is lost.

The session is stored locally at `~/.phelix/session.json`.

```bash
# Interactive
phelix auth login

# Non-interactive
phelix auth login --username <user> --apiKey <key>

phelix auth status     # show current session + expiry
phelix auth logout     # invalidate and remove the session
```

> **Tip:** want to use the CLI entirely offline, or just don't need the
> dashboard? Skip `auth login` — build/run commands work the same. Only the
> dashboard upload is skipped.

## Agent-scoped sessions for the monitor daemon (recommended)

By default, `phelix auth login` issues a **full-scope** session — valid for the
dashboard and every monitoring channel. On a server you only monitor, that is
more power than the daemon needs: a compromised server would expose a token that
can touch your whole account.

Use the **agent-scoped** variant when provisioning servers (install scripts,
systemd setup):

```bash
phelix auth login --username <user> --apiKey <key> --scope agent
```

This requests a monitoring-only token (stored separately at
`~/.phelix/agent-session.json` so it never displaces your interactive session).
The token is accepted by the monitoring gRPC channel and nothing else —
dashboards, account settings, and session management all reject it. If the
server is ever compromised, the stolen token cannot be used to take over your
account; revoke it from any other machine with `phelix auth logout` (which logs
out both sessions).

Both sessions can coexist: a typical server runs `--scope agent` for the daemon,
while you use a normal (full-scope) login for interactive CLI commands there.
`phelix auth status` shows both, including each token's scope.

## Backend rate limits

The backend throttles failed logins (HTTP 429 with a `Retry-After` window) and
abusive retry patterns on the monitoring channel (gRPC `ResourceExhausted`). The
CLI honors these: a throttled login tells you when to retry instead of failing
generically, and the monitor daemon backs off for the announced window rather
than hammering the backend. If you see a "too many failed authentication
attempts" or "too many login attempts" message, wait for the stated window —
retrying earlier only extends it.

## Related

- [Monitoring](monitoring.md) — what the monitor daemon transmits, and per-app
  watching.
- [Security](security.md) — session storage and transport security.
- [TLS & certificate policy](../architecture/backend-contracts/tls-cert-policy.md)
  — the agent gRPC channel's transport security model.
- Error codes and exit codes for auth failures:
  [error-codes](../reference/error-codes.md), [exit-codes](../reference/exit-codes.md).
