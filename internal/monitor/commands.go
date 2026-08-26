package monitor

import (
	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
)

// appCommandExecutor executes lifecycle commands issued by the backend.
type appCommandExecutor struct{}

// NewCommandExecutor returns the default CommandExecutor implementation. It is
// transport-agnostic and can be reused by any monitoring transport (gRPC today,
// previously WebSocket).
func NewCommandExecutor() CommandExecutor {
	return &appCommandExecutor{}
}

// resolvedApp is the identity of a managed app resolved by name or ID.
type resolvedApp struct {
	ID   string
	Name string
	Port int
}

// resolveApp finds a managed app by name or ID, mirroring how CLI commands
// resolve an identifier.
func resolveApp(identifier string) (*resolvedApp, error) {
	apps := app.Manager.ListApplications()
	for _, a := range apps {
		if a.Name == identifier || a.ID == identifier {
			return &resolvedApp{ID: a.ID, Name: a.Name, Port: a.Port}, nil
		}
	}
	return nil, phelixerr.Newf(phelixerr.CodeNotFound, "app not found: %s", identifier)
}

// Execute runs a lifecycle command against a managed application.
//
// The common types (start/stop/restart) are dispatched in-process through the
// app manager so they work regardless of the daemon's environment — in
// particular under systemd, where the PATH may not include the directory the
// phelix binary lives in. Unknown types fall back to a `phelix <type> <id>`
// subprocess, preserving the previous behavior for any future command.
func (e *appCommandExecutor) Execute(cmd Command) error {
	target, err := resolveApp(cmd.Payload.AppName)
	if err != nil {
		return err
	}

	logs.Info("monitor", "executing command '%s' for app '%s' (ID: %s)", cmd.Payload.Type, target.Name, target.ID)

	switch cmd.Payload.Type {
	case "start":
		// Reuse the app's persisted port so `start` never overrides it with a
		// default.
		return app.Manager.StartApplication(target.ID, target.Port, target.Name)
	case "stop":
		return app.Manager.StopApplication(target.ID)
	case "restart":
		return app.Manager.RestartApplication(target.ID)
	case "remove":
		return app.Manager.RemoveApplication(target.ID)
	}

	return e.execFallback(cmd, target.ID)
}

// execFallback shells out to `phelix <type> <id>` for command types the app
// manager does not handle in-process.
func (e *appCommandExecutor) execFallback(cmd Command, appID string) error {
	execCmd, err := newPhelixCommand(cmd.Payload.Type, appID)
	if err != nil {
		return err
	}
	if err := runPhelixCommand(execCmd); err != nil {
		// %w (not %v) so the inner exit status remains inspectable.
		return phelixerr.Wrap(phelixerr.CodeProcessFailed, "command failed", err)
	}
	return nil
}
