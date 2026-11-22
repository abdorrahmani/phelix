# Interactive usage

Phelix can guide you through a task when you run it in a terminal. Open the
complete menu with `phelix wizard`, or run a command and follow the prompts for
any missing details.

## Choose a task

```bash
phelix wizard
```

The menu includes every available command except `security` and `log`. Choose
a command, then choose an action if it has subcommands. Type a command name to
filter the menu, use the arrow keys to move, and press Enter to select it.
Choose Cancel to leave without running a task.

Commands that both run a task and have subcommands offer a separate Run choice.
For example, the deployment menu offers Run deploy and unlock. Commands with
nothing to ask, such as version and list, run their usual action immediately.
Running `phelix` without a command also opens the menu in a terminal.

## Follow a command's prompts

```bash
phelix rebuild
phelix watch
phelix scale
phelix env
phelix health add
phelix secrets set
phelix session show
phelix controller runtime
```

Application choices come from the running Controller. Each choice shows the
application name and ID. Rebuild asks which application to rebuild even when
the current directory contains a named `phelix.yaml`. An explicit application
argument skips that choice.

| Task | Missing details Phelix asks for |
| --- | --- |
| build | Application name, with the project name offered as a default; port when the project does not supply one |
| rebuild, start, stop, restart, status, remove, watch | Application; start also offers All applications |
| deploy, deploy unlock | Application after choosing the action |
| rollback | Application, previous version, optional reason, then confirmation after the preview |
| scale | Application and replica count |
| health | Action and application; path for set; endpoint name and URL for add; endpoint name for remove |
| autoscale | Action and application; reason for pause |
| hooks | Action and application; execution ID for inspect |
| jobs | Action and application for run, history, and logs |
| env | Action, application, and variable name or assignment |
| secrets | Action, secret name when needed, application, and a private value for set |
| dockerize | Application, image tag, whether to push, and registry when pushing |
| inspect, context, build-report, deployment-report | Application when the selected view uses one |
| plan, operation, authz, session | Action and missing plan, operation, or session ID; checkpoint note if no other details were supplied |
| controller | Action and application or operation ID when needed |
| webhook status, webhook history | Application |
| auth login | Missing username or API key |

Supplied arguments and flags keep their values. Normal command validation
still applies to what you enter. A cancelled prompt stops the task before
execution. Rollback with no previous version prints a message and does nothing.

Builds for workers, jobs, or a build matrix do not ask for a port. Workspace
requests using `--all` or `--workspace`, and builds using `--resume`, retain
their existing scope and do not ask for an application. With `--json`, prompts
appear on stderr so stdout remains available for the result.

## Private input

API keys and secret values are hidden while you type. Secret values are sent
to the Controller as input, rather than as command arguments. If you supply
`--from-file` to secrets set, Phelix uses the file without reading terminal
input. Environment assignments entered through the wizard are also hidden
while you type.

## Scripts and services

Piping input, redirecting stdin, or running without a terminal disables these
wizard prompts. Commands keep their usual arguments, defaults, and errors.
The `security` command and its subcommands, and `log` including its `logs`
alias, remain outside this wizard flow.

The dedicated `phelix matrix init` wizard runs in the CLI so it can guide you
through editing the project's matrix settings. Management actions continue
to run through the Controller. Application choices require the Controller
to be running.

## Related

- [Matrix builds](matrix-builds.md)
- [Command reference](../reference/commands.md)
- [Controller](controller.md)
