# Phelix - Go/Rust Application Manager

## Overview
Phelix is a powerful Go Application Manager that helps you build, run, and manage Go applications across multiple servers. It provides a comprehensive set of commands for managing your Go applications with features like authentication, monitoring, and application lifecycle management.

## Features
- Multi-server application management
- Real-time application monitoring
- Centralized logging
- Cross-server status checking
- Authentication and security
- Application lifecycle management
- WebSocket-based monitoring service
- **Encrypted environment variable management**
- **Zero-downtime blue-green and rolling deploys** (via `phelix proxy`)
- **Versioned builds with zero-downtime rollback** (all builds create versioned artifacts; `--tag` for meaningful labels)

## Installation
The CLI requires Go to be installed on your system. If Go is not installed, Phelix will attempt to install it automatically on Linux systems. For other operating systems, you'll need to install Go manually.

```bash
# Install Phelix
go install github.com/abdorrahmani/phelix@latest
```

## Authentication
Before using most commands, you need to authenticate with the Phelix service.

### Authentication Commands

#### `phelix auth`
Authenticates with phelix.anophel.com using your username and API key.
```bash
phelix auth
```
You will be prompted to enter:
- Username
- API Key

#### `phelix auth status`
Shows your current authentication status, including:
- Authenticated user
- Session ID
- Expiration time

#### `phelix auth logout`
Logs out the current user and removes the session.

## Application Management Commands

### Building and Running Applications

#### `phelix build <NAME> --port <PORT>`
Builds and runs a Go application from the current directory. Every successful build creates a new versioned artifact (`v1`, `v2`, ...) — not just zero-downtime builds.
```bash
phelix build myapp --port 8080
phelix build myapp --port 8080 --tag "pre-holiday-release"
```
- `NAME`: Name of your application
- `--port`: Port to run the application on (default: 8080)
- `--tag`: Optional label stored as metadata alongside the auto-incremented version (e.g. `"hotfix-auth-bug"`). Tags are informational only — version IDs (`v1`, `v2`, ...) remain the source of truth.

#### `phelix rebuild <ID> --port <PORT>`
Rebuilds and runs an existing application. Every successful rebuild creates a new versioned artifact.
```bash
phelix rebuild 123 --port 8080
phelix rebuild 123 --port 8080 --tag "hotfix-auth-bug"
```
- `ID`: Application ID
- `--port`: Port to run the application on (defaults to previous port if unspecified)
- `--tag`: Optional label stored as metadata alongside the auto-incremented version

For zero-downtime rebuilds, see [Zero-Downtime Deploys](#zero-downtime-deploys) below (`--blue-green` / `--replicas`).

### Application Control

#### `phelix start <ID> --port <PORT>`
Starts a specific application.
```bash
phelix start 123 --port 8080
```

#### `phelix stop <ID>`
Stops a running application.
```bash
phelix stop 123
```

#### `phelix restart <ID>`
Restarts an application.
```bash
phelix restart 123
```

#### `phelix remove <ID>`
Removes an application from Phelix.
```bash
phelix remove 123
```

### Application Information

#### `phelix list`
Lists all applications across all monitored servers, showing:
- ID
- Name
- Version (current version from `versions.json`, e.g. `v3 (hotfix-auth)`; `—` for apps with no version history)
- Status
- PID
- Uptime
- Deploy method (`blue-green`, `rolling`, or classic)
- Proxy enrollment (public port and active slot, when the proxy daemon is up)

#### `phelix status <ID>`
Shows detailed status of a specific application, including:
- ID
- Name
- Status
- PID
- Uptime
- RAM Usage (MB)
- CPU Usage (%)
- **Version section**: current version (with tag if set), git commit hash, build timestamp, deploy timestamp, binary size
- **Recent version history**: last 3 versions with tags and `*` marking the current one
- Deploy method, active slot/replicas, and health tier (when using zero-downtime deploys)
- Active version number (e.g. `v3`)
- Last rollback info (from/to version and timestamp)
- Live proxy routing (public port, primary backend, in-flight requests)

#### `phelix log <ID>`
Displays logs for a specific application.
- Shows the last 10 lines of historical logs
- Streams new logs in real-time
- Press Ctrl+C to exit
- Supports cross-server log viewing

### Environment Variable Management

Phelix provides a secure way to manage encrypted environment variables for your applications. All environment variables are encrypted using AES-256-GCM encryption and stored in `~/.phelix/envs/`.

#### Master Key Management
The master key is automatically generated and stored at `~/.phelix/master.key`. Keep this file safe and never commit it to version control.

#### `phelix env set <AppName> <KEY=VALUE> [KEY=VALUE ...]`
Sets one or more encrypted environment variables for an application.
```bash
# Set a single variable
phelix env set myapp DATABASE_URL=postgresql://localhost/db

# Set multiple variables at once
phelix env set myapp API_KEY=xxx SECRET_TOKEN=yyy DEBUG=true
```

#### `phelix env get <AppName> <KEY>`
Retrieves a specific environment variable value.
```bash
phelix env get myapp DATABASE_URL
```
Note: Sensitive keys (containing SECRET, KEY, TOKEN, PASSWORD) are masked in output.

#### `phelix env list <AppName>`
Lists all environment variables set for an application.
```bash
phelix env list myapp
```
Output shows variable names with masked values for security.

#### `phelix env unset <AppName> <KEY>`
Removes an environment variable from an application.
```bash
phelix env unset myapp DEBUG
```

#### Environment Variable Injection
When an application starts, Phelix automatically:
1. Decrypts the .env.enc file using the master key
2. Injects all environment variables into the application process
3. The application receives variables exactly as you set them

#### Security Features
- **Encrypted Storage**: All values are encrypted with AES-256-GCM
- **Sensitive Key Masking**: Keys containing SECRET, KEY, TOKEN, PASSWORD are automatically masked in logs
- **Secure Key Storage**: Master key is stored with restricted file permissions (0600)
- **No Plain Text**: Environment variables are never stored in plain text
- **Application Isolation**: Each application has its own encrypted environment file

### Zero-Downtime Deploys

Phelix can rebuild and cut over without dropping connections by keeping a reverse-proxy daemon on the app's **stable public port** and switching traffic to a newly built, health-checked instance.

```
Client → :8080 [phelix proxy]  ──atomic target──→ blue  :9001
                                               ↘ green :9002
```

#### Prerequisites
1. Your app must listen on the port given by the `PORT` environment variable (Phelix sets this for each internal instance).
2. Prefer a real health endpoint so deploys use Tier 1 checks:
   ```bash
   phelix health set myapp --path /health
   ```
   Without one, Phelix falls back to Tier 2 (any HTTP response) or Tier 3 (TCP only) and prints a warning.

#### `phelix proxy`
Starts the reverse-proxy daemon **in the background** (control socket: `~/.phelix/proxy.sock`).
```bash
phelix proxy              # detach into background
phelix proxy status       # show enrolled apps and routing
phelix proxy stop         # drain and stop the daemon
phelix proxy --foreground # run attached (for systemd / debugging)
```
`phelix rebuild --blue-green` / `--replicas` will also auto-start the daemon if it is not already running.

#### Blue-green deploy
Builds a new binary, starts it on the inactive slot (blue ↔ green), waits until healthy, then atomically switches the proxy. The previous instance is drained and stopped.
```bash
phelix rebuild myapp --blue-green
phelix rebuild myapp --blue-green --port 8080
```

#### Rolling deploy
Restarts N replicas one at a time (never more than one down). Useful when you want capacity during the cut-over.
```bash
phelix rebuild myapp --replicas 3
```

#### What you see in `list` / `status`
- **Deploy**: `blue-green (blue|green)` or `rolling (×N)`, or `-` for classic stop→start rebuilds
- **Proxy**: `on :<public> → <slot>` when enrolled, otherwise `off`

#### Failure behaviour
If the new instance fails its health check, the deploy **aborts**, the new instance is killed, and the currently active instance is left untouched — public traffic keeps flowing.

### Versioned Builds and Rollback

Every successful build — whether via `phelix build`, `phelix rebuild`, or zero-downtime deploy — creates a numbered version (`v1`, `v2`, `v3`, ...) rather than overwriting. Versions store both the binary and its paired encrypted env snapshot, so rollback always restores a known-good binary + env pair — never binary-only.

#### Build-and-deploy ordering guarantee

Versions use a two-phase commit to ensure safety:

1. **Build succeeds** → version is created on disk (`builds/vN/binary`, `env/vN.enc`) and recorded in `versions.json` with `is_current: false`.
2. **Deploy succeeds** (start/health check passes) → `PromoteVersion` flips `is_current: true` and updates the `current` symlink.
3. **Deploy fails** → the version exists on disk for inspection or retry, but `is_current` stays `false` and the `current` symlink is never moved. The active running instance is untouched.

This means you can always inspect a failed build's artifacts, but a broken deploy can never corrupt the "current" pointer.

```
~/.phelix/apps/myapp/
├── builds/v1/binary
├── builds/v2/binary
├── builds/v3/binary
├── env/v1.enc
├── env/v2.enc
├── current -> builds/v3    (symlink, updated only after proxy switch succeeds)
├── versions.json           (metadata per version)
└── rollback.log            (audit trail)
```

#### `phelix rollback <AppName>`
Roll back to a previous version. The rollback path depends on how the app was deployed:

- **Zero-downtime apps** (built with `--blue-green` or `--replicas`): rollback goes through the same deploy path — health check, proxy switch, graceful shutdown — so you get the same zero-downtime guarantee.
- **Classic apps** (built with plain `phelix build` / `phelix rebuild`): rollback stops the current instance, copies the versioned binary into place, and starts it. This is a brief downtime rollback (stop → start).

```bash
phelix rollback myapp              # roll back to the previous version
phelix rollback myapp --to v2      # roll back to a specific version
phelix rollback myapp --to 3       # version number without 'v' prefix also works
phelix rollback myapp --to hotfix-auth-bug   # roll back by tag name
```

The `--to` flag accepts either a version ID (`v3`, `3`) or a unique tag name. If a tag matches exactly one version, it resolves automatically. If a tag matches zero or more than one version, an error is returned — use a version ID to disambiguate.

#### `phelix rollback <AppName> --list`
Show all retained versions with metadata.
```bash
phelix rollback myapp --list
```
Output table columns:
- **Version** — `vN` label
- **Tag** — optional label (e.g. `hotfix-auth-bug`), if provided via `--tag`
- **Commit** — git commit hash (if available at build time)
- **Built** — build timestamp (RFC 3339)
- **Size** — binary size on disk
- **Current** — whether this version is actively serving traffic
- **Prune soon** — whether this version would be removed after the next build (based on retention policy)

#### How rollback works
Rollback supports two paths depending on how the app was originally deployed:

**Zero-downtime path** (blue-green / rolling apps):
1. `ResolveVersionOrTag` resolves the `--to` argument to a concrete version ID (supports version numbers and unique tag names)
2. `ExistingVersionSource` resolves the binary and env paths for the target version
3. A new instance starts on the inactive slot (blue or green)
4. Tiered health checks verify the instance is healthy
5. The proxy atomically switches traffic to the new instance
6. The old instance is gracefully drained and stopped
7. The `current` symlink and `versions.json` are updated

If the rollback target fails its health check, the rollback **aborts** and the active instance is left untouched — identical to a failed forward deploy.

**Classic path** (apps built without `--blue-green` / `--replicas`):
1. `ResolveVersionOrTag` resolves the target version
2. The current instance is stopped
3. The versioned binary is copied to the app's expected location
4. The app is started via `app.Manager.StartApplication`
5. The version is promoted (`is_current` set to true, `current` symlink updated)

If the start fails, the version exists on disk but `is_current` stays false — the user can retry without a broken "current" pointer.

#### Rollback safety
- **Concurrent protection**: A deploy lock prevents rollback from racing with another deploy or rollback on the same app
- **Versioned env**: Binary and env are paired per version; rollback always restores both
- **Audit log**: Every rollback attempt (success or failure) is recorded in `~/.phelix/apps/<AppName>/rollback.log`

#### Retention policy
Old versions are automatically pruned after each successful build, keeping the last 5 versions by default. The currently active version is never pruned, even if it falls outside the retention window. The retention count is configurable per plan tier (Free: 3, Pro: 10, Enterprise: unlimited).

### Multi-Server Monitoring

#### `phelix monitor`
Starts the WebSocket monitoring service that:
- Monitors application status across all servers
- Sends application information to the central server
- Automatically reconnects if connection is lost
- Provides real-time updates for all managed applications

#### Server Management
Phelix can monitor multiple servers simultaneously. Each server running Phelix will:
- Register itself with the central monitoring service
- Send regular status updates
- Maintain its own application state
- Sync with other servers when needed

## System Requirements
- Go programming language
- Internet connection for authentication and monitoring
- Sufficient permissions to create and manage application files
- Network access between servers (if monitoring multiple servers)

## File Locations
- Session file: `~/.phelix/session.json`
- Log files: `~/.phelix/logs/phelix.log`
- Application logs: Stored in the application's directory
- Deploy instance logs: `~/.phelix/logs/deploy_*.log`
- Deploy state: `~/.phelix/apps/<AppName>/deploy.json`
- Version metadata: `~/.phelix/apps/<AppName>/versions.json`
- Versioned binaries: `~/.phelix/apps/<AppName>/builds/vN/binary`
- Versioned env snapshots: `~/.phelix/apps/<AppName>/env/vN.enc`
- Rollback audit log: `~/.phelix/apps/<AppName>/rollback.log`
- Current symlink: `~/.phelix/apps/<AppName>/current` → `builds/vN`
- Proxy control socket: `~/.phelix/proxy.sock`
- Server configuration: `~/.phelix/config.json`
- **Master key: `~/.phelix/master.key`** (Keep this safe!)
- **Encrypted environment files: `~/.phelix/envs/<appid>.env.enc`**

## Error Handling
- Authentication errors will prompt you to run `phelix auth`
- Build errors will be displayed with detailed output
- Connection errors will be logged and retried automatically
- Server communication errors will be handled gracefully

## Best Practices
1. Always authenticate before using the CLI
2. Use meaningful names for your applications
3. Monitor application logs for debugging
4. Use the status command to check application health
5. Keep your session active by logging in when needed
6. Ensure proper network connectivity between servers
7. Regularly check server status across your infrastructure
8. Monitor resource usage across all servers
9. **Use encrypted environment variables for sensitive data** (API keys, database credentials, etc.)
10. **Never commit master keys or encrypted env files to version control**
11. **Regularly rotate sensitive credentials**
12. **Use descriptive variable names** (e.g., DATABASE_CONNECTION_URL instead of DB)
13. **Use `phelix rollback --list` to review available versions** before rolling back
14. **Keep the proxy daemon running** (`phelix proxy`) for zero-downtime rollbacks
15. **Use `--tag` to label important builds** (e.g. `--tag "v2.1-release"`) for easier rollback identification
16. **Check `phelix status <app>` for version history** — the last 3 versions are shown so you can see what you'd roll back to

## Security Considerations
- All communication is encrypted
- Authentication tokens are securely stored
- Server-to-server communication is authenticated
- Regular session validation
- Secure file permissions

## Version Information
Current version: 0.0.1
To check the version:
```bash
phelix version
```

## Support
For support and issues, please visit:
- GitHub Issues: [github.com/abdorrahmani/phelix/issues](https://github.com/abdorrahmani/phelix/issues)
- Documentation: [phelix.anophel.com/docs](https://phelix.anophel.com/docs)

## License
This project is licensed under the MIT License - see the LICENSE file for details. 