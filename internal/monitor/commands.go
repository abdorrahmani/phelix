package monitor

import (
	"strconv"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/project"
)

// appCommandExecutor executes lifecycle commands issued by the backend.
type appCommandExecutor struct{}

// RollbackHandler executes a remote rollback command through the SAME
// service layer the local `phelix rollback` CLI command uses. The cmd
// package registers the real implementation at daemon startup
// (SetRollbackHandler); the nil default makes the executor's behavior
// explicit — without a handler a rollback command is rejected, never
// approximated by the lifecycle fallback below.
var RollbackHandler func(payload CommandPayload) error

// SetRollbackHandler registers the remote rollback implementation. Called by
// the monitor daemon wiring; kept in monitor so the executor stays decoupled
// from the cmd package (import cycle).
func SetRollbackHandler(h func(payload CommandPayload) error) {
	RollbackHandler = h
}

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
	case project.StrategyClassic, project.StrategyBlueGreen,
		project.StrategyCanary, project.StrategyProgressive:
		// Canary/progressive resolve their rollout plan from the app's
		// phelix.yaml (deploy.rollout.*), the same file a local rebuild in
		// that directory reads; no extra payload fields are needed.
		return []string{"--strategy", payload.Strategy}, nil
	case project.StrategyRolling:
		args := []string{"--strategy", payload.Strategy}
		if payload.Replicas > 0 {
			args = append(args, "--replicas", strconv.Itoa(payload.Replicas))
		}
		return args, nil
	}
	return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
		"unsupported deployment strategy %q: expected one of: classic, blue-green, rolling, canary, progressive", payload.Strategy)
}

// Execute runs a lifecycle command against a managed application.
//
// Classic apps take start/stop/restart/remove in-process through the app
// manager so they work regardless of the daemon's environment. Apps managed by
// a zero-downtime strategy do not: their instances, internal ports and proxy
// route live in deploy.json, and the single-PID app manager would kill the
// serving instance and then try to bind the proxy-owned public port — leaving
// the app down while reporting success. Those go through the CLI, whose
// start/stop/restart/remove already tear down and restore a deployment
// (cmd/lifecycle.go), exactly as a local invocation would.
//
// Unknown types also fall back to `phelix <type> <id>`, preserving the previous
// behavior for any future command.
func (e *appCommandExecutor) Execute(cmd Command) error {
	// Remote rollback never reaches the lifecycle dispatch: it invokes the
	// existing rollback engine through the registered handler (same service
	// layer as the local CLI), so there is no second implementation and no
	// shelling out to `phelix rollback`. A rollback carrying deployment
	// overrides is a backend bug — rejected, never silently ignored.
	if cmd.Payload.Type == CommandRollback {
		if cmd.Payload.Strategy != "" || cmd.Payload.Replicas != 0 {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"deployment overrides do not apply to rollback commands")
		}
		if RollbackHandler == nil {
			return phelixerr.New(phelixerr.CodeUnimplemented,
				"rollback command not available: no rollback handler registered")
		}
		return RollbackHandler(cmd.Payload)
	}

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

	if state := deploy.LoadZeroDowntime(target.Name); state != nil {
		logs.Info("monitor", "app '%s' is deploy-managed (mode %s); running '%s' through the CLI",
			target.Name, state.Mode, cmd.Payload.Type)
	} else {
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
	}

	return e.execFallback(cmd, target, overrideArgs)
}

// execFallback shells out to `phelix <type> <id> [flags]` for command types the
// app manager does not handle in-process. It runs in the app's directory so
// `phelix rebuild` finds that app's phelix.yaml, the same file a local rebuild
// in that directory would read.
func (e *appCommandExecutor) execFallback(cmd Command, target *resolvedApp, extraArgs []string) error {
	args := append([]string{cmd.Payload.Type, target.ID}, extraArgs...)
	execCmd, err := NewPhelixCommand(target.Directory, args...)
	if err != nil {
		return err
	}
	if err := runPhelixCommand(execCmd); err != nil {
		// %w (not %v) so the inner exit status remains inspectable.
		return phelixerr.Wrap(phelixerr.CodeProcessFailed, "command failed", err)
	}
	return nil
}
