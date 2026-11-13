package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/abdorrahmani/phelix/internal/authz"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/hooks"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/project"
)

// RemoteHookCommand executes one validated hooks_run command.
func RemoteHookCommand(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	if req.GetType() != phelixgrpc.HooksCommandRun {
		return remoteHookError(phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"unknown hook command type %q", req.GetType()))
	}

	info, err := GetAppInfo(req.GetAppName())
	if err != nil {
		return remoteHookError(err)
	}
	cfg, _, err := hooksForApp(info)
	if err != nil {
		return remoteHookError(err)
	}
	name := req.GetHooks().GetName()
	if !isLifecycleHookName(name) {
		return remoteHookError(phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"unknown lifecycle hook %q", name))
	}
	if cfg == nil || cfg.Hooks == nil || cfg.Hooks.Get(name) == nil {
		return remoteHookError(phelixerr.Newf(phelixerr.CodeNotFound,
			"hook %q is not configured for %s", name, info.Name))
	}
	hook := cfg.Hooks.Get(name)
	if !hook.IsEnabled() {
		return remoteHookError(phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"hook %q is disabled for %s", name, info.Name))
	}

	version, _ := deploy.CurrentVersion(info.Name)
	deploymentID := fmt.Sprintf("manual-%d", time.Now().UnixNano())
	adapter, runner := newHookRunner(info.Name, info.ID, info.Directory, deploymentID, "manual", cfg.Hooks, version, 0)
	if adapter == nil || runner == nil {
		return remoteHookError(phelixerr.New(phelixerr.CodeConfiguration,
			"hook engine is unavailable"))
	}

	result := runner.Run(context.Background(), name, hook)
	if result.Execution != nil {
		phelixgrpc.SendHookExecutionEvent(phelixgrpc.HookExecutionProto(result.Execution))
	}
	logs.Info("hooks", "remote %s for %s finished with status %s (request %s)",
		name, info.Name, hookStatus(result.Execution), req.GetRequestId())

	out := &pb.MonitorCommandResult{Status: "success"}
	if result.Execution != nil {
		out.HookExecution = phelixgrpc.HookExecutionProto(result.Execution)
	}
	if result.Failed() {
		out.Status = "error"
		out.ErrorCode = string(phelixerr.CodeProcessFailed)
		if result.Err != nil {
			out.Error = result.Err.Error()
			out.ErrorCode = string(phelixerr.CodeOf(result.Err))
		} else {
			out.Error = fmt.Sprintf("%s hook failed", name)
		}
	}
	return out
}

func isLifecycleHookName(name string) bool {
	for _, candidate := range project.HookNames {
		if candidate == name {
			return true
		}
	}
	return false
}

func hookStatus(execution *hooks.Execution) string {
	if execution == nil {
		return "unknown"
	}
	return execution.Status
}

func remoteHookError(err error) *pb.MonitorCommandResult {
	return &pb.MonitorCommandResult{
		Status:    "error",
		Error:     err.Error(),
		ErrorCode: string(phelixerr.CodeOf(err)),
	}
}

// remoteHookAuthorize gates hooks_run through the host authorization boundary
// with the dedicated hook action, before any ledger entry or execution.
func remoteHookAuthorize(req *pb.MonitorCommandRequest) error {
	info, err := GetAppInfo(req.GetAppName())
	if err != nil {
		return err
	}
	return authorizeDirectExecution(authz.ActionHook, info.Name, info.ID)
}
