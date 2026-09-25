# Installation

Phelix ships as a single binary. The installer detects your OS and architecture
and downloads a matching prebuilt binary, so you do **not** need a Go or Rust
toolchain just to install Phelix (those are only needed to *build your apps*).

Phelix targets Linux, with experimental Windows support.

## Recommended — one-line installer

```bash
curl -fsSL https://phelix.anophel.com/install.sh | bash
```

The installer:

- **detects your operating system and architecture** and downloads the matching
  prebuilt binary,
- installs it to `/usr/local/bin/phelix`, and
- on Linux, registers and enables a `phelix.service` **systemd** unit that runs
  `phelix monitor` directly.

The monitor daemon runs in the foreground: it restores the managed apps that
were previously running and keeps a persistent TLS-secured gRPC connection to
the backend, reconnecting with exponential backoff.

Verify the install:

```bash
phelix version              # verify the install
sudo systemctl status phelix   # Linux: monitor service running?
```

## Runtime dependencies and system requirements

- **Go** toolchain (`go`) and/or **Rust** toolchain (`cargo`) — required to
  build apps. Phelix detects a missing toolchain and offers to install it
  automatically on Linux; for other operating systems it prints manual
  instructions.
- **Docker** — only required for `phelix dockerize` (and for the
  [Docker runtime](../guides/docker-runtime.md)).
- **Internet connection** — optional. Only needed for authentication and the
  `phelix.anophel.com` dashboard; build/run works fully offline.
- Sufficient permissions to create and manage application files.
- Network access between servers, if you monitor multiple servers.

## Updating

```bash
phelix update            # download and install the latest release
phelix update --check    # only report whether an update is available
```

`phelix update` upgrades the Phelix CLI/agent binary itself to the latest
stable release, using the **same release server and layout as the one-line
installer** (`https://phelix.anophel.com/releases/<version>/phelix-<os>-<arch>`),
so no Go/Rust toolchain is needed. In summary it:

- Resolves the latest stable version and compares it against the running one
  with proper semantic-version ordering (`v1.2.3` and `1.2.3` are the same
  version; a locally newer build is never downgraded).
- Downloads the matching prebuilt binary for the current platform.
- Verifies the release's **SHA-256 checksum** when one is published — a mismatch
  aborts the update. When no checksum is published, it warns and continues (same
  policy as the installer).
- Validates the download is a genuine Phelix binary for this platform, then
  **atomically replaces** the installed executable (the old binary is preserved
  until the new one is proven in place, and restored automatically if a later
  step fails).
- On Linux, if the `phelix.service` systemd unit is running, it **restarts the
  monitor service** afterwards. `KillMode=process` means the restart stops only
  the monitor — managed applications keep running. A service that is installed
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
version history) is **never modified** — the command only replaces the binary
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

- [Quick Start](quick-start.md) — build and deploy your first app.
- [The PORT Contract](the-port-contract.md) — the one hard requirement for
  managed apps.
- [Configuration reference](../reference/configuration.md) — the `phelix.yaml`
  project file.
