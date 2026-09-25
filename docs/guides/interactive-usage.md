# Interactive Usage

Phelix has two complementary interactive modes so you never have to memorize
exact argument order or flag names.

## Guided menu — `phelix wizard`

Run `phelix wizard` to open a survey-driven menu that walks you through any task
step by step: build, rebuild, roll back, start/stop/restart, status, logs, list,
environment variables, health checks, dockerize, deploy unlock, auth, and
version. Each choice routes to the same logic the plain subcommand uses, so
behavior is identical.

```bash
phelix wizard
```

## Auto-prompting on every command

When you run a command without a required positional argument (or key flag),
Phelix prompts for the missing input instead of erroring — **as long as stdin is
a terminal**.

```bash
phelix rebuild            # → prompts: which app?
phelix rollback           # → prompts: which app? then which version?
phelix env               # → prompts: set/get/list/unset/check, app, key…
phelix health set         # → prompts: which app? then the health path
phelix dockerize          # → prompts: which app? tag? push? registry?
```

Affected commands: `build`, `rebuild`, `rollback`, `start`, `stop`, `restart`,
`status`, `remove`, `dockerize`, `env`, `health *`, `deploy unlock`, and
`auth login` (which uses masked password input).

> **Scriptable by design.** Piping input or running in CI (non-TTY stdin)
> disables all prompts — commands return their normal usage/missing-argument
> error and exit code, so existing automation keeps working unchanged. The
> long-running daemons `monitor` and `proxy` (foreground) are not interactive.

## Related

- [Matrix builds](matrix-builds.md) — `phelix matrix init` is a dedicated
  interactive wizard for configuring a build matrix.
- [Command reference](../reference/commands.md) — the non-interactive form of
  every command.
