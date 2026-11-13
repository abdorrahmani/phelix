package cmd

import (
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/hooks"
	"github.com/abdorrahmani/phelix/internal/project"
)

func TestRemoteHookCommandRunsConfiguredHook(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := writeJobProject(t, "hooked", "name: hooked\nport: 8080\nhooks:\n  pre_deploy:\n    command: [\"sh\", \"-c\", \"echo remote-hook\"]\n")
	seedJobApp(t, "hooked", dir)

	result := RemoteHookCommand(&pb.MonitorCommandRequest{
		RequestId: "r1", Type: phelixgrpc.HooksCommandRun, AppName: "hooked",
		Hooks: &pb.HookOptions{Name: project.HookPreDeploy},
	})
	if result.GetStatus() != "success" || result.GetHookExecution() == nil {
		t.Fatalf("result = %+v", result)
	}
	execution := result.GetHookExecution()
	if execution.GetHook() != project.HookPreDeploy || execution.GetStatus() != hooks.StatusSucceeded {
		t.Fatalf("execution = %+v", execution)
	}
	if execution.GetDeploymentId() == "" || execution.GetExecutionId() == "" {
		t.Fatalf("execution identity = %+v", execution)
	}
	if execution.GetStdout() == "" {
		t.Fatalf("stdout was not captured: %+v", execution)
	}

	records, err := hooks.NewStore("hooked").List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 1 || records[0].ID != execution.GetExecutionId() {
		t.Fatalf("records = %+v", records)
	}
}

func TestRemoteHookCommandReportsFailedHook(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := writeJobProject(t, "failing", "name: failing\nport: 8080\nhooks:\n  pre_deploy:\n    command: [\"sh\", \"-c\", \"exit 7\"]\n")
	seedJobApp(t, "failing", dir)

	result := RemoteHookCommand(&pb.MonitorCommandRequest{
		RequestId: "r1", Type: phelixgrpc.HooksCommandRun, AppName: "failing",
		Hooks: &pb.HookOptions{Name: project.HookPreDeploy},
	})
	if result.GetStatus() != "error" || result.GetHookExecution() == nil {
		t.Fatalf("result = %+v", result)
	}
	if result.GetHookExecution().GetStatus() != hooks.StatusFailed {
		t.Fatalf("execution = %+v", result.GetHookExecution())
	}
	if result.GetHookExecution().GetHasExitCode() != true || result.GetHookExecution().GetExitCode() != 7 {
		t.Fatalf("exit code = %+v", result.GetHookExecution())
	}
}

func TestRemoteHookCommandRejectsUnavailableHook(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := writeJobProject(t, "plain", "name: plain\nport: 8080\nhooks:\n  pre_deploy:\n    command: [\"true\"]\n    enabled: false\n")
	seedJobApp(t, "plain", dir)

	disabled := RemoteHookCommand(&pb.MonitorCommandRequest{
		RequestId: "r1", Type: phelixgrpc.HooksCommandRun, AppName: "plain",
		Hooks: &pb.HookOptions{Name: project.HookPreDeploy},
	})
	if disabled.GetStatus() != "error" || disabled.GetErrorCode() != string(phelixerr.CodeInvalidArgument) {
		t.Fatalf("disabled = %+v", disabled)
	}

	unconfigured := RemoteHookCommand(&pb.MonitorCommandRequest{
		RequestId: "r2", Type: phelixgrpc.HooksCommandRun, AppName: "plain",
		Hooks: &pb.HookOptions{Name: project.HookPostDeploy},
	})
	if unconfigured.GetStatus() != "error" || unconfigured.GetErrorCode() != string(phelixerr.CodeNotFound) {
		t.Fatalf("unconfigured = %+v", unconfigured)
	}

	unknown := RemoteHookCommand(&pb.MonitorCommandRequest{
		RequestId: "r3", Type: phelixgrpc.HooksCommandRun, AppName: "plain",
		Hooks: &pb.HookOptions{Name: "not_a_hook"},
	})
	if unknown.GetStatus() != "error" || unknown.GetErrorCode() != string(phelixerr.CodeInvalidArgument) {
		t.Fatalf("unknown = %+v", unknown)
	}
}

func TestRemoteHookAuthorizeResolvesApp(t *testing.T) {
	dir := writeJobProject(t, "hooked", "name: hooked\nport: 8080\n")
	withJobManager(t, map[string]*app.AppInfo{
		"app-hooked": {ID: "app-hooked", Name: "hooked", Directory: dir},
	})
	if err := remoteHookAuthorize(&pb.MonitorCommandRequest{AppName: "hooked"}); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if err := remoteHookAuthorize(&pb.MonitorCommandRequest{AppName: "missing"}); err == nil {
		t.Fatal("missing app must fail authorization")
	}
}
