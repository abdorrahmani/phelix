# Best Practices

1. Log in (`phelix auth login`) to push metrics, logs, and events to your
   dashboard; build/run works fine without it.
2. Use meaningful, unique names for your applications.
3. Monitor application logs (`phelix log`) for debugging.
4. Use `phelix status` to check application health regularly.
5. Log in once and keep your session active — apps you create while logged out are
   synced to the dashboard the next time you log in.
6. Ensure proper network connectivity between servers.
7. Regularly check server status across your infrastructure.
8. Monitor resource usage (RAM/CPU) across all servers.
9. **Use encrypted environment variables for sensitive data** (API keys, database
   credentials, etc.).
10. **Never commit master keys or encrypted env files to version control.**
11. **Regularly rotate sensitive credentials.**
12. **Use descriptive variable names** (e.g., `DATABASE_CONNECTION_URL` instead of
    `DB`).
13. **Use `phelix rollback --list`** to review available versions before rolling
    back.
14. **Preview destructive rollbacks with `--dry-run`** before executing —
    especially for production rollbacks, large rollback distances (many versions
    behind), tagged-release rollbacks, rollbacks after a failed deployment, and
    blue-green/rolling apps where the traffic transition matters:
    ```bash
    phelix rollback myapp --to stable --dry-run   # inspect the plan
    phelix rollback myapp --to stable             # then execute
    ```
15. **Keep the proxy daemon running** (`phelix proxy`) for zero-downtime
    rollbacks.
16. **Use `--tag`** to label important builds (e.g. `--tag "v2.1-release"`) for
    easier rollback identification.
17. **Check `phelix status <app>`** for version history before deciding to roll
    back.
18. **Use `phelix dockerize`** to containerize apps with optimized, cached
    Dockerfiles.
19. **Watch the automatic Build Report after every build** — it is the fastest way
    to detect unexpected binary growth, compilation regressions, or toolchain
    changes (the compiler version in the report makes accidental toolchain bumps
    visible).
20. **Treat repeated duration regressions in the same cache mode as a signal**: if
    cold builds keep getting slower across versions, the codebase — not the cache —
    is the problem.
21. **Use `phelix build-report <AppName>`** to review stored build history before
    investigating a performance or size issue; it is read-only and works offline.
22. **After a matrix build, check each combination's summary** — a size regression
    in one platform/toolchain combination won't show up in the others.

> The original README numbered two items "20"; both are preserved above,
> renumbered to keep every item.

## Related

- [Rollback](rollback.md), [Build reports](build-reports.md),
  [Environment variables](environment-variables.md), [Security](security.md).
