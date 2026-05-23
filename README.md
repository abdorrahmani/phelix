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
Builds and runs a Go application from the current directory.
```bash
phelix build myapp --port 8080
```
- `NAME`: Name of your application
- `--port`: Port to run the application on (default: 8080)

#### `phelix rebuild <ID> --port <PORT>`
Rebuilds and runs an existing application.
```bash
phelix rebuild 123 --port 8080
```
- `ID`: Application ID
- `--port`: Port to run the application on (defaults to previous port if unspecified)

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
- Status
- PID
- Uptime
- Server location

#### `phelix status <ID>`
Shows detailed status of a specific application, including:
- ID
- Name
- Status
- PID
- Uptime
- RAM Usage (MB)
- CPU Usage (%)
- Server information
- Last update time

#### `phelix log <ID>`
Displays logs for a specific application.
- Shows the last 10 lines of historical logs
- Streams new logs in real-time
- Press Ctrl+C to exit
- Supports cross-server log viewing

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
- Server configuration: `~/.phelix/config.json`

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