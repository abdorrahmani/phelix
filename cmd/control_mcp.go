package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/controlapi"
	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	"github.com/abdorrahmani/phelix/internal/controller"
	"github.com/abdorrahmani/phelix/internal/machine"
	phelixmcp "github.com/abdorrahmani/phelix/internal/mcp"
)

type controlMCPServices struct{}

func callControlMCP(ctx context.Context, argv []string) (*machine.Envelope, error) {
	cfg, err := controller.LoadClientConfig()
	if err != nil {
		return nil, err
	}
	connectionCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	connection, client, err := controlapi.Connect(connectionCtx, cfg.Socket)
	cancel()
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	directory, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	operation, err := client.Execute(ctx, &pb.CommandRequest{Argv: append(argv, "--json"), Directory: directory, Caller: "mcp"})
	if err != nil {
		return nil, err
	}
	stream, err := client.WatchOperation(ctx, &pb.WatchRequest{Id: operation.Id})
	if err != nil {
		return nil, err
	}
	var output strings.Builder
	for {
		event, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil && operation.Cancellable {
				cancelCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
				client.CancelOperation(cancelCtx, &pb.OperationRequest{Id: operation.Id})
				stop()
			}
			return nil, err
		}
		if !event.Stderr {
			if output.Len()+len(event.Output) > 4<<20 {
				return nil, fmt.Errorf("Controller MCP result exceeds 4 MiB")
			}
			output.WriteString(event.Output)
		}
		if event.Operation != nil && event.Operation.CompletedAt != "" {
			if output.Len() > 0 {
				return mcpDecodeEnvelope([]byte(output.String()))
			}
			return machine.Failure(&controlOperationError{event.Operation}, int(event.Operation.ExitCode), ""), nil
		}
	}
}

func (controlMCPServices) Context(ctx context.Context, in phelixmcp.ContextInput) (*machine.Envelope, error) {
	return callControlMCP(ctx, append([]string{"context", "--versions=" + strconv.Itoa(in.Versions), "--operations=" + strconv.Itoa(in.Operations), "--log-lines=" + strconv.Itoa(in.LogLines)}, appArg(in.App)...))
}

func (controlMCPServices) Inspect(ctx context.Context, in phelixmcp.InspectInput) (*machine.Envelope, error) {
	argv := []string{"inspect", in.Resource}
	if in.Resource == "operations" {
		argv = append(argv, "--app="+in.App, "--limit="+strconv.Itoa(in.Limit))
	} else if in.Resource == "versions" {
		argv = append(argv, "--limit="+strconv.Itoa(in.Limit))
	}
	if in.Resource == "app" || in.Resource == "deployment" || in.Resource == "versions" || in.Resource == "health" {
		argv = append(argv, appArg(in.App)...)
	}
	return callControlMCP(ctx, argv)
}

func (controlMCPServices) PlanShow(ctx context.Context, in phelixmcp.PlanShowInput) (*machine.Envelope, error) {
	return callControlMCP(ctx, []string{"plan", "show", in.PlanID})
}

func (controlMCPServices) PlanList(ctx context.Context, in phelixmcp.PlanListInput) (*machine.Envelope, error) {
	limit, err := mcpPlanListLimit(in.Limit)
	if err != nil {
		return machine.Failure(err, ExitCodeFor(err), ""), nil
	}
	return callControlMCP(ctx, []string{"plan", "list", "--app=" + in.App, "--limit=" + strconv.Itoa(limit)})
}

func (controlMCPServices) OperationStatus(ctx context.Context, in phelixmcp.OperationStatusInput) (*machine.Envelope, error) {
	return callControlMCP(ctx, []string{"operation", "status", in.OperationID})
}

func (controlMCPServices) PlanApply(ctx context.Context, in phelixmcp.PlanApplyInput) (*machine.Envelope, error) {
	return callControlMCP(ctx, []string{"plan", "apply", in.PlanID})
}

func (controlMCPServices) PlanCreate(ctx context.Context, in phelixmcp.PlanCreateInput) (*machine.Envelope, error) {
	var commandArgv []string
	switch in.Action {
	case phelixmcp.ActionRebuild:
		commandArgv = controlArguments(mcpSyntheticRebuildCreateCmd(in), nil)
	case phelixmcp.ActionRollback:
		commandArgv = controlArguments(mcpSyntheticRollbackCreateCmd(in), nil)
	default:
		return nil, fmt.Errorf("unsupported MCP plan action")
	}
	return callControlMCP(ctx, append(append([]string{"plan", "create", in.Action}, appArg(in.App)...), commandArgv...))
}

func (controlMCPServices) SessionCreate(ctx context.Context, in phelixmcp.SessionCreateInput) (*machine.Envelope, error) {
	return callControlMCP(ctx, []string{"session", "create", "--title=" + in.Title, "--app=" + in.App, "--project=" + in.Project})
}

func (controlMCPServices) SessionShow(ctx context.Context, in phelixmcp.SessionShowInput) (*machine.Envelope, error) {
	return callControlMCP(ctx, []string{"session", "show", in.SessionID, "--no-resolve=" + strconv.FormatBool(in.NoResolve)})
}

func (controlMCPServices) SessionList(ctx context.Context, in phelixmcp.SessionListInput) (*machine.Envelope, error) {
	limit, err := mcpSessionListLimit(in.Limit)
	if err != nil {
		return machine.Failure(err, ExitCodeFor(err), ""), nil
	}
	return callControlMCP(ctx, []string{"session", "list", "--status=" + in.Status, "--app=" + in.App, "--limit=" + strconv.Itoa(limit)})
}

func sessionControlLinks(argv []string, plans, operations, deployments []string) []string {
	for _, pair := range []struct {
		name string
		ids  []string
	}{{"plan", plans}, {"operation", operations}, {"deployment", deployments}} {
		for _, id := range pair.ids {
			argv = append(argv, "--"+pair.name+"="+id)
		}
	}
	return argv
}

func (controlMCPServices) SessionCheckpoint(ctx context.Context, in phelixmcp.SessionCheckpointInput) (*machine.Envelope, error) {
	return callControlMCP(ctx, sessionControlLinks([]string{"session", "checkpoint", in.SessionID, "--note=" + in.Note, "--step=" + in.Step}, in.Plans, in.Operations, in.Deployments))
}

func (controlMCPServices) SessionComplete(ctx context.Context, in phelixmcp.SessionCompleteInput) (*machine.Envelope, error) {
	return callControlMCP(ctx, sessionControlLinks([]string{"session", "complete", in.SessionID, "--result=" + in.Result}, in.Plans, in.Operations, in.Deployments))
}

func (controlMCPServices) SessionFail(ctx context.Context, in phelixmcp.SessionFailInput) (*machine.Envelope, error) {
	return callControlMCP(ctx, sessionControlLinks([]string{"session", "fail", in.SessionID, "--reason=" + in.Reason}, in.Plans, in.Operations, in.Deployments))
}
