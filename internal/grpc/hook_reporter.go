package grpc

import (
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/hooks"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/server"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

// SendHookExecutionEvent delivers one immutable attempt over the monitor
// stream.
func SendHookExecutionEvent(event *pb.HookExecutionState) {
	if event == nil || event.GetExecutionId() == "" {
		return
	}
	if err := monitorStream.send(&pb.MonitorEvent{
		ServerId:  server.GetServerID(),
		Timestamp: event.GetStartedAt(),
		Payload:   &pb.MonitorEvent_HookExecution{HookExecution: event},
	}); err != nil {
		logs.WarningFile("grpc", "[gRPC Hook] failed to send execution event execution_id=%s: %v",
			event.GetExecutionId(), err)
	}
}

// sendHookSnapshots pushes the resolved hook configuration of every watched
// app. An app whose file has no hooks sends an empty snapshot so a removed
// section converges on the backend.
func (c *Client) sendHookSnapshots() {
	for _, a := range app.Manager.ListApplications() {
		if !a.Watching {
			continue
		}
		cfg, err := project.Load(a.Directory)
		if err != nil || cfg == nil {
			continue
		}
		snapshot := &pb.HookConfigSnapshot{AppId: a.ID, AppName: a.Name}
		if cfg.Hooks != nil {
			for _, name := range cfg.Hooks.Configured() {
				hook := cfg.Hooks.Get(name)
				snapshot.Hooks = append(snapshot.Hooks, &pb.HookConfigEntry{
					Name: name, Command: hook.Command, Timeout: hook.TimeoutDuration().String(),
					Retries: int32(hook.Retries), RetryInterval: hook.RetryIntervalDuration().String(),
					ContinueOnError: hook.ContinuesOnError(name), WorkingDir: hook.WorkingDir,
					Enabled: hook.IsEnabled(),
				})
			}
		}
		if err := monitorStream.send(&pb.MonitorEvent{
			ServerId:  server.GetServerID(),
			Timestamp: time.Now().UnixMilli(),
			Payload:   &pb.MonitorEvent_HookConfig{HookConfig: snapshot},
		}); err != nil {
			logs.ErrorFile("grpc", "[gRPC Hook] failed to send config snapshot for %s: %v", a.Name, err)
			return
		}
	}
}

// HookExecutionProto converts a durable attempt into its wire form.
func HookExecutionProto(execution *hooks.Execution) *pb.HookExecutionState {
	if execution == nil {
		return nil
	}
	state := &pb.HookExecutionState{
		ExecutionId:     execution.ID,
		AppId:           execution.AppID,
		AppName:         execution.AppName,
		DeploymentId:    execution.DeploymentID,
		Strategy:        execution.Strategy,
		Hook:            execution.Hook,
		Attempt:         int32(execution.Attempt),
		Version:         int32(execution.Version),
		PreviousVersion: int32(execution.PreviousVersion),
		Command:         execution.Command,
		WorkingDir:      execution.WorkingDir,
		Status:          execution.Status,
		StartedAt:       execution.StartedAt.UnixMilli(),
		FinishedAt:      execution.FinishedAt.UnixMilli(),
		DurationMs:      execution.DurationMS,
		Stdout:          execution.Stdout,
		Stderr:          execution.Stderr,
		StdoutTruncated: execution.StdoutTruncated,
		StderrTruncated: execution.StderrTruncated,
		FailureReason:   execution.FailureReason,
		TimeoutMs:       execution.TimeoutMS,
		ContinueOnError: execution.ContinueOnError,
		OwnerPid:        int32(execution.OwnerPID),
	}
	if execution.ExitCode != nil {
		state.ExitCode = int32(*execution.ExitCode)
		state.HasExitCode = true
	}
	return state
}

// sendHookExecutionResync replays the most recent attempts of every watched
// app after a reconnect. Upserts by execution id make the replay idempotent.
func (c *Client) sendHookExecutionResync() {
	for _, a := range app.Manager.ListApplications() {
		if !a.Watching {
			continue
		}
		executions, err := hooks.NewStore(a.Name).List()
		if err != nil {
			continue
		}
		if len(executions) > 20 {
			executions = executions[:20]
		}
		for _, execution := range executions {
			SendHookExecutionEvent(HookExecutionProto(execution))
		}
	}
}
