# Container status and app selector fixes

Completed on November 21, 2026, in `/home/anophel/Projects/Phelix`.

## Problems and causes

The seven applications managed by the shared Docker Controller were running,
but `phelix list` and the first row of `phelix status` displayed `stopped`, PID
zero, missing uptime, and zero resource usage. The detailed deployment section
already recognized the running containers.

The commands checked deployment status correctly, then reloaded the app registry.
That reload checked a host PID from inside the Controller container. The host
process was outside the Controller's process view, so the reload reset the app
to stopped. The two sections therefore described the same application differently.

The app selectors still existed in the local command handlers. Controller
routing replaced those handlers with a request to the Controller. The receiving
command had no terminal input, so commands without an app argument rejected the
request before they could show a selector.

## Completed changes

- Every registry reload now reads the deployment's verified status, serving PID,
  start time, and public port. The shared read path keeps list, status, and other
  registry readers consistent across repeated reloads.
- The first status row gets Docker memory and CPU readings for the serving
  container. Missing readings return an error instead of inventing zero usage.
- Running and partially running deployments show uptime. A partially running
  replica set retains its `degraded` status. Stopped deployments show PID zero
  and `N/A` uptime. Unknown start times also show `N/A`.
- The first live replica remains the source of its PID and start time even when
  its recorded PID is zero. A later replica no longer replaces that start time.
- Ownership checks for older containers read the registry directly. They retain
  the registered-app and known-container checks while avoiding a recursive
  registry reload that would hang while the registry was already locked.
- The CLI now shows the Controller's app list on the caller's terminal for
  argument-free `status` and `remove`. Choices are sorted and display names with
  IDs. The selected ID is sent to the existing command handler.
- Explicit app arguments, workspace status, nested commands, and command flags
  retain their existing behavior. Empty app lists, failed requests, invalid
  choices, and cancellation end before an action is submitted.
- The restored selector sends its menu to stderr for `status --json`, preserving
  stdout for the JSON result.

The existing removal and purge actions were reused. No dependencies, configuration
options, or inline code comments were added.

## Verification

The full repository suite passed:

```bash
go test ./... -count=1 -timeout=240s
```

The affected code also passed:

```bash
go vet ./cmd ./internal/app ./internal/deploy
```

After the final JSON adjustment, the focused selector and JSON checks passed:

```bash
go test ./cmd -run 'TestControl(AppSelection|Routes|Leaves)|Test.*JSON' -count=1 -timeout=30s
```

Added checks cover blue-green, rolling, and worker deployment reads; repeated
reloads; app names and IDs; current and legacy ownership labels; missing Docker
metrics; stopped containers; partial replica availability; missing start times;
duplicate app names; unnamed apps; selection errors; cancellation; explicit
arguments; workspace status; nested commands; and flag preservation.

For a container check, a separate copy of app registry and deployment records
was made from the backend Controller. A temporary program exercised the updated
Controller command entry point inside the existing Docker runtime image. It used
the actual Docker daemon to inspect the running service containers. No credentials
or environment values were copied, and no second Controller daemon was started.

All seven apps passed `list --json` and `status --json` checks by both name and
ID. The first status row agreed with the active slot, and its PID matched Docker.
The following values were observed during that check. Uptime and memory naturally
change after collection.

| App | Status | Host PID | Uptime | Memory |
| --- | --- | ---: | --- | ---: |
| contact | running | 2566326 | 00h 34m 49s | 30.0 MB |
| notification | running | 2562920 | 00h 35m 22s | 38.0 MB |
| ticket | running | 2570329 | 00h 34m 59s | 35.0 MB |
| telegram | running | 2568793 | 00h 35m 19s | 19.0 MB |
| billing | running | 2660263 | 00h 25m 37s | 13.6 MB |
| admin | running | 2565082 | 00h 36m 06s | 36.8 MB |
| tracking | running | 2567575 | 00h 36m 07s | 29.4 MB |

The live CPU readings were zero for idle apps. A controlled Docker response
verified that a nonzero reading of 12.5 percent is carried into the status row.
The separate state copy did not include a proxy socket, so proxy and traffic
sections were outside that container check.

A temporary CLI executable was also exercised on a real terminal against the
backend Controller. `status` displayed the choices and successfully selected
billing. `remove` displayed the choices, and Ctrl+C cancelled it with all seven
apps retained. A live application was not removed to test the menu.

During the final check, the backend Controller had been recreated and returned
an empty application list. This task did not restart that Controller or change
its registry. The seven-app values above describe the earlier verification.
With the empty list, the updated CLI's terminal `status --json` returned one
valid `NOT_FOUND` JSON envelope. No state restoration was attempted.

## Local commits

| Commit | Change |
| --- | --- |
| `1bad531` | `fix(status): preserve Docker app status across state reloads` |
| `b61e9d7` | `fix(cli): restore status and remove app selectors through the controller` |
| `9df1e39` | `test(status): cover missing start times and selector flags` |
| `a574e6d` | `fix(cli): keep app selection menus out of JSON output` |

The guide and this report are committed separately. All commits are local. This
task did not push changes, replace the backend's installed executable, or restart
its Controller. Unrelated working-tree changes were left outside these commits.

## Testing the reviewed changes in b-phelix

After reviewing the source, build the development executable from this checkout
and refresh only the Controller image:

```bash
cd /home/anophel/Projects/b-phelix
./scripts/build-phelix.sh dev
docker compose build controller
docker compose up -d --no-deps --wait controller
docker compose exec controller phelix list
docker compose exec controller phelix status billing
docker compose exec controller phelix status
docker compose exec controller phelix remove
```

Cancel the removal selector with Ctrl+C unless you intend to remove the selected
app. The commands above retain the existing Controller state volume. Both the
updated CLI and updated Controller are needed because status is read by the
Controller's command process. These upgrade commands were documented for your
review and were not executed by this task.

For replica deployments, the top PID, uptime, and resource readings describe the
selected serving instance, consistent with the existing single-process status
row. They are not totals across replicas. Docker resource readings require access
to the Docker daemon. End-to-end removal of a live backend app, proxy recovery,
and a production rollout were not performed.
