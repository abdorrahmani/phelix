# Server package security

Use `phelix security server` to inspect operating system packages on the current host. Application dependency commands under `phelix security` keep their existing behavior.

## Collect and inspect a snapshot

```bash
phelix security server scan
phelix security server scan --full --json
phelix security server status
phelix security server inventory --limit 100 --offset 0
phelix security server inventory --package openssl
phelix security server history --limit 20
phelix security server inventory --scan-id <scan-id> --json
```

Collection reads the package database without refreshing repositories or changing installed packages. `--full` makes the default full-inventory behavior explicit. Interrupt a scan with Ctrl+C. `--timeout 5m` bounds the operation. JSON output uses the standard Phelix result envelope; progress goes to stderr. Failed and cancelled collection attempts remain in history. An inventory alone does not establish that packages are safe.

## Supported systems and prerequisites

| Distribution | Utilities | Inventory |
| --- | --- | --- |
| Ubuntu, Debian | apt, dpkg-query | dpkg installed records |
| Fedora, RHEL, Rocky Linux, AlmaLinux | dnf, rpm | RPM query format |
| Arch Linux | pacman, vercmp | Pacman local database |
| Alpine Linux | apk | APK installed database |
| openSUSE, Leap, Tumbleweed | zypper, rpm | RPM query format |

Distribution identity comes from os-release. Unsupported distributions and unavailable utilities are reported explicitly. Read access to os-release and the package database is required. Root is normally unnecessary for collection. Run the command in the host environment whose packages you want to inspect; a container sees its own operating system and installed packages.

Full distribution version strings, epochs, architecture, source packages, and multiple installed versions are preserved. dpkg entries that only retain removed-package configuration are excluded. A virtual package is retained as a provider relationship rather than invented as an installed package. Metadata absent from the installed database, such as repositories and pin state, is shown as unknown.

## Storage and scripting

Snapshots and immutable scan history are stored in the `host-security` directory under the Phelix data directory. `PHELIX_DATA_DIR` selects that directory. Protect it as private operational data. Package identity snapshots are reused when inventory is unchanged. Queries support `--limit` from 1 to 500 and a nonnegative `--offset`. Keep the scan ID fixed when paging through an inventory.

Successful inventory commands exit 0. Invalid arguments, missing snapshots, failed collection, and cancellation use the standard Phelix error codes documented in the [exit code reference](../reference/exit-codes.md). A successful inventory does not mean vulnerability checks succeeded or that the server is secure.

If collection fails, check that the native utility is installed, the package database is readable, and the distribution version exists in os-release. Phelix does not repair package databases or remove package-manager locks. Output size and package count are bounded; exceeding a bound fails collection instead of presenting truncated data as complete.
