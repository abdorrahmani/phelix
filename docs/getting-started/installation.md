# Installation

Phelix ships as a single binary. The installer detects your OS and architecture
and downloads a matching prebuilt binary, so you do **not** need a Go or Rust
toolchain just to install Phelix (those are only needed to *build your apps*).

Phelix targets Linux, with experimental Windows support.

## Recommended one-line installer

```bash
curl -fsSL https://phelix.anophel.com/install.sh | bash
```

The installer:

- **detects your operating system and architecture** and downloads the matching
  prebuilt binary,
- installs it to `/usr/local/bin/phelix`, and
- on Linux, registers and enables a `phelix.service` **systemd** unit that runs
  `phelix controller` directly.

The Controller owns local application management, recovery, the proxy and
monitoring. The CLI connects to it through a protected Unix socket. Backend
monitoring is optional and uses the existing authenticated TLS gRPC stream.
`phelix monitor` remains a compatibility alias that starts this same service.
Do not start a second Controller alongside the installed service.

Verify the install:

```bash
phelix version              # verify the install
sudo systemctl status phelix   # Linux: Controller service running?
```

## Beta releases and pinned versions

The default installer uses the stable channel. To install the latest beta:

```bash
curl -fsSL https://phelix.anophel.com/install.sh | bash -s -- --channel beta
```

You can also select beta through the installer's environment:

```bash
curl -fsSL https://phelix.anophel.com/install.sh | PHELIX_CHANNEL=beta bash
```

To install a specific release, pass its complete version:

```bash
curl -fsSL https://phelix.anophel.com/install.sh | bash -s -- --version 1.3.0-beta.1
```

`--version` takes precedence over channel selection and accepts a leading `v`.
`PHELIX_VERSION` provides the same option through the installer's environment.
`--channel` takes precedence over `PHELIX_CHANNEL`. Only `stable` and `beta`
are supported. Beta versions use `-beta` or `-beta.1`, `-beta.2`, and so on.

The installer resolves stable releases from `releases/latest/version` and beta
releases from `releases/beta/version`, then downloads from the concrete version
directory. It prefers the `X-Phelix-Version` header and otherwise uses the first
body line. Missing, invalid, or incompatible channel metadata stops installation
before the installed binary is changed. A pinned version does not need channel
metadata.

## Runtime dependencies and system requirements

- **Go** toolchain (`go`) and/or **Rust** toolchain (`cargo`), required to
  build apps. Phelix detects a missing toolchain and offers to install it
  automatically on Linux; for other operating systems it prints manual
  instructions.
- **Docker**, only required for `phelix dockerize` (and for the
  [Docker runtime](../guides/docker-runtime.md)).
- **Internet connection**, optional. Only needed for authentication and the
  `phelix.anophel.com` dashboard; build/run works fully offline.
- Sufficient permissions to create and manage application files.
- Network access between servers, if you monitor multiple servers.

## Updating

```bash
phelix update            # download and install the latest release
phelix update --check    # only report whether an update is available
```

To check for or install a beta update:

```bash
phelix update --channel beta --check
phelix update --channel beta
```

Channel selection applies to the current command. Even after installing a beta,
`phelix update` defaults to stable. Use `--channel beta` whenever you want a beta
update. A final release is newer than its own beta, and beta numbers are compared
numerically, so `beta.10` is newer than `beta.2`. The updater never downgrades: if
the selected channel is older than the installed version, it leaves it in place.

`phelix update` upgrades the Phelix CLI/agent binary itself to the latest
stable release, using the **same release server and layout as the one-line
installer** (`https://phelix.anophel.com/releases/<version>/phelix-<os>-<arch>`),
so no Go/Rust toolchain is needed. In summary it:

- Resolves the latest version in the chosen channel and compares it against the running one
  with proper semantic-version ordering (`v1.2.3` and `1.2.3` are the same
  version; a locally newer build is never downgraded).
- Downloads the matching prebuilt binary for the current platform.
- Verifies the release's **SHA-256 checksum** when one is published, a mismatch
  aborts the update. When no checksum is published, it warns and continues (same
  policy as the installer).
- Validates the download is a genuine Phelix binary for this platform, then
  **atomically replaces** the installed executable (the old binary is preserved
  until the new one is proven in place, and restored automatically if a later
  step fails).
- On Linux, if the `phelix.service` systemd unit is running, it **restarts the
  Controller service** afterwards. `KillMode=process` means the restart stops only
  the Controller; managed applications keep running. A service that is installed
  but stopped is left stopped; hosts without systemd (or macOS) simply get the
  binary replacement.
- Replacing a binary under `/usr/local/bin` needs root: `phelix update` detects
  this, authenticates `sudo` interactively when a terminal is attached, and
  fails with an actionable error otherwise. It never asks for root when the
  binary lives somewhere you can already write.

If Phelix is already up to date:

```text
✓ Phelix is already up to date (v1.2.3).
```

Application state under `~/.phelix/` (sessions, builds, environment variables,
version history) is **never modified**, the command only replaces the binary
itself and never rebuilds your applications.

> The internal mechanics of the update/release pipeline (checksum policy,
> atomic replacement, version resolution, the release-server layout) are
> documented for contributors in
> [development/release-process.md](../development/release-process.md).

## Uninstalling

```bash
curl -fsSL https://phelix.anophel.com/install.sh | bash -s -- --uninstall
```

The uninstaller (same one-line script, with `--uninstall`) reverses the install
step by step:

- **Stops and disables** the `phelix.service` systemd unit on Linux (the service
  is stopped, disabled, the unit file at `/etc/systemd/system/phelix.service` is
  removed, and `systemctl daemon-reload` is run). No-op where there is no systemd
  (e.g. macOS).
- **Removes the binary** from `/usr/local/bin/phelix` (or your `--install-dir`).
- **Removes the legacy** `phelix-startup.sh` wrapper left behind by older
  installs, if present.

```bash
sudo systemctl status phelix   # should be gone (Linux)
command -v phelix              # should print nothing
```

> **Note:** managed apps and all state under `~/.phelix/` are intentionally left
> in place, so you can reinstall later without losing your apps. Remove the data
> directory manually if you want a clean slate:
>
> ```bash
> rm -rf ~/.phelix
> ```

## Next steps

- [Quick Start](quick-start.md), build and deploy your first app.
- [The PORT Contract](the-port-contract.md), the one hard requirement for
  managed apps.
- [Configuration reference](../reference/configuration.md), the `phelix.yaml`
  project file.

## Controller service and Docker installation

The systemd service now runs `phelix controller`; `phelix monitor` is a
compatibility alias. Start the Controller before lifecycle CLI commands. The
existing identity, registry and deployment state stay in the original data
namespace. Stop the old service before upgrading and retain the full state.

For a Controller container, build the repository's `Dockerfile.controller`, mount
persistent state and a shared Unix socket directory, mount only required source
roots, and attach the Controller to the applications' user-defined network.
Docker socket access grants administrative control over the Docker environment.
No management TCP port is required. Follow the complete
[Container example](../../examples/docker-container-controller/README.md) and
[Controller operations guide](../guides/controller.md).
