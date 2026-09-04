package monitor

import (
	"strconv"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/project"
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
	ID        string
	Name      string
	Port      int
	Directory string
}

// resolveApp finds a managed app by name or ID, mirroring how CLI commands
// resolve an identifier.
func resolveApp(identifier string) (*resolvedApp, error) {
	apps := app.Manager.ListApplications()
	for _, a := range apps {
		if a.Name == identifier || a.ID == identifier {
			return &resolvedApp{ID: a.ID, Name: a.Name, Port: a.Port, Directory: a.Directory}, nil
		}
	}
	return nil, phelixerr.Newf(phelixerr.CodeNotFound, "app not found: %s", identifier)
}

// rebuildOverrideArgs translates a backend-issued one-off deployment override
// into `phelix rebuild` flags. No override adds no flags, so the rebuild
// resolves its strategy from the app's phelix.yaml exactly as a local rebuild
// does; nothing here is ever written back to that file.
//
// An override the CLI cannot honor exactly is rejected rather than degraded:
// the error travels back as a MonitorCommandResult, so a backend and agent
// that disagree about the strategy vocabulary say so instead of quietly
// deploying something else. The checks mirror project.Config.validate() —
// replicas belong to rolling only.
func rebuildOverrideArgs(payload CommandPayload) ([]string, error) {
	if payload.Strategy == "" && payload.Replicas == 0 {
		return nil, nil
	}
	if payload.Type != "rebuild" {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"deployment overrides apply to a rebuild command only, got %q", payload.Type)
	}
	if payload.Replicas < 0 {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"invalid replicas %d: must be >= 1", payload.Replicas)
	}
	if payload.Replicas > 0 && payload.Strategy != project.StrategyRolling {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"replicas requires the rolling strategy, got strategy %q", payload.Strategy)
	}

	switch payload.Strategy {
	case project.StrategyClassic, project.StrategyBlueGreen:
		return []string{"--strategy", payload.Strategy}, nil
	case project.StrategyRolling:
		args := []string{"--strategy", payload.Strategy}
		if payload.Replicas > 0 {
			args = append(args, "--replicas", strconv.Itoa(payload.Replicas))
		}
		return args, nil
	}
	return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
		"unsupported deployment strategy %q: expected one of classic, blue-green, rolling", payload.Strategy)
}

// Execute runs a lifecycle command against a managed application.
//
// The common types (start/stop/restart) are dispatched in-process through the
// app manager so they work regardless of the daemon's environment — in
// particular under systemd, where the PATH may not include the directory the
// phelix binary lives in. Unknown types fall back to a `phelix <type> <id>`
// subprocess, preserving the previous behavior for any future command.
func (e *appCommandExecutor) Execute(cmd Command) error {
	// Validated before dispatch, not inside the fallback: the in-process
	// branches below would otherwise accept an override and ignore it.
	overrideArgs, err := rebuildOverrideArgs(cmd.Payload)
	if err != nil {
		return err
	}

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

	return e.execFallback(cmd, target, overrideArgs)
}

// execFallback shells out to `phelix <type> <id> [flags]` for command types the
// app manager does not handle in-process. It runs in the app's directory so
// `phelix rebuild` finds that app's phelix.yaml, the same file a local rebuild
// in that directory would read.
func (e *appCommandExecutor) execFallback(cmd Command, target *resolvedApp, extraArgs []string) error {
	args := append([]string{cmd.Payload.Type, target.ID}, extraArgs...)
	execCmd, err := newPhelixCommand(target.Directory, args...)
	if err != nil {
		return err
	}
	if err := runPhelixCommand(execCmd); err != nil {
		// %w (not %v) so the inner exit status remains inspectable.
		return phelixerr.Wrap(phelixerr.CodeProcessFailed, "command failed", err)
	}
	return nil
}
