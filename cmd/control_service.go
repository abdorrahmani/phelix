package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/controlapi"
	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	"github.com/abdorrahmani/phelix/internal/controller"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/monitor"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/version"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func validateControlCommand(root *cobra.Command, cfg controller.Config, request *pb.CommandRequest) (bool, error) {
	if request.Caller != "" && request.Caller != "mcp" {
		return false, fmt.Errorf("unsupported management caller")
	}
	if len(request.Argv) == 0 || len(request.Argv) > 256 {
		return false, fmt.Errorf("management command required")
	}
	for _, arg := range request.Argv {
		if len(arg) > 1<<20 || strings.ContainsRune(arg, 0) {
			return false, fmt.Errorf("invalid command argument")
		}
	}
	command, _, err := root.Find(request.Argv)
	if err != nil || !managedCommand(command) {
		return false, fmt.Errorf("command is not available through the management API")
	}
	if strings.Contains(command.CommandPath(), "proxy stop") {
		return false, fmt.Errorf("proxy lifecycle belongs to Controller; stop or restart the Controller service")
	}
	for _, argument := range request.Argv {
		if argument == "--foreground" && command.Name() == "proxy" {
			return false, fmt.Errorf("run phelix controller to supervise the proxy")
		}
	}
	if request.Directory == "" {
		request.Directory = server.DataDir()
	}
	if command.Name() == "build" || command.Name() == "dockerize" || command.Name() == "rebuild" || command.Name() == "deploy" || command.Name() == "apply" || command.Name() == "retry" {
		source, err := cfg.SourcePath(request.Directory)
		if command.Name() == "deploy" || command.Name() == "rebuild" {
			for _, arg := range request.Argv[1:] {
				if strings.HasPrefix(arg, "-") {
					continue
				}
				if item, lookupErr := GetAppInfo(arg); lookupErr == nil {
					source, err = cfg.SourcePath(item.Directory)
					break
				}
			}
		}
		if err != nil {
			return false, err
		}
		request.Directory = source
	} else {
		if _, err := os.Stat(request.Directory); err != nil {
			request.Directory = server.DataDir()
		}
	}
	for i, arg := range request.Argv {
		if strings.HasPrefix(arg, "--source-dir=") {
			source, err := cfg.SourcePath(strings.TrimPrefix(arg, "--source-dir="))
			if err != nil {
				return false, err
			}
			request.Argv[i] = "--source-dir=" + source
		}
		if arg == "--source-dir" {
			if i+1 >= len(request.Argv) {
				return false, fmt.Errorf("--source-dir requires a path")
			}
			source, err := cfg.SourcePath(request.Argv[i+1])
			if err != nil {
				return false, err
			}
			request.Argv[i+1] = source
		}
	}
	cancellable := command.Name() == "log" || command.Name() == "logs" || command.Name() == "rebuild" || command.Name() == "build"
	return cancellable, nil
}

func startControlService(command *cobra.Command, owner *controller.Controller) (func(), error) {
	operations, err := controlapi.NewOperations(context.Background(), filepath.Join(server.DataDir(), "controller-operations"), owner.Config.MaxOperations, owner.ExecuteAdapter)
	if err != nil {
		return nil, err
	}
	cache := monitor.NewRuntimeMetricsCache()
	metricsCtx, cancelMetrics := context.WithCancel(context.Background())
	metricsDone := make(chan struct{})
	go func() { defer close(metricsDone); cache.Run(metricsCtx) }()
	phelixgrpc.SetMetricsCollector(monitor.NewMetricsCollector(cache))
	var validationMu sync.Mutex
	service := &controlapi.Service{
		Operations:   operations,
		InfoResponse: &pb.ControllerInfo{AgentId: owner.Identity, ServerId: owner.Identity, Mode: owner.Config.Mode, Version: version.Version, Socket: owner.Config.Socket},
		Validate: func(request *pb.CommandRequest) (bool, error) {
			validationMu.Lock()
			defer validationMu.Unlock()
			return validateControlCommand(command.Root(), owner.Config, request)
		},
		QueryHandler: func(ctx context.Context, request *pb.QueryRequest) (*pb.QueryResponse, error) {
			response, err := queryControl(ctx, request)
			if err != nil {
				return nil, err
			}
			if request.Kind == pb.QueryKind_METRICS {
				for _, application := range response.Applications {
					for _, instance := range application.Instances {
						if instance.ContainerId == "" || instance.Status != "running" {
							continue
						}
						sample, ok := cache.Get(instance.ContainerId)
						if !ok {
							return nil, status.Error(codes.Unavailable, "Docker metrics not yet available or stale; retry after the next sample")
						}
						response.Metrics = append(response.Metrics, &pb.Metrics{ApplicationId: application.Id, ContainerId: sample.ContainerID, CpuPercent: sample.CPUPercent, MemoryBytes: sample.MemoryBytes, MemoryLimit: sample.MemoryLimit, NetworkRx: sample.NetworkRX, NetworkTx: sample.NetworkTX, BlockRead: sample.BlockRead, BlockWrite: sample.BlockWrite, State: sample.State, RestartCount: sample.RestartCount, Health: sample.Health, ExitCode: int32(sample.ExitCode)})
					}
				}
			}
			return response, nil
		},
	}
	uids := append([]uint32{0, uint32(os.Getuid())}, owner.Config.AllowedUIDs...)
	var socketUID []int
	if owner.Config.SocketUID != nil {
		socketUID = append(socketUID, *owner.Config.SocketUID)
		uids = append(uids, uint32(*owner.Config.SocketUID))
	}
	if owner.Config.SocketGID != nil {
		if len(socketUID) == 0 {
			socketUID = append(socketUID, os.Getuid())
		}
		socketUID = append(socketUID, *owner.Config.SocketGID)
	}
	listener, err := controlapi.Listen(owner.Config.Socket, uids, service, socketUID...)
	if err != nil {
		cancelMetrics()
		<-metricsDone
		operations.Close()
		return nil, err
	}
	return func() { listener.Close(); operations.Close(); cancelMetrics(); <-metricsDone }, nil
}

func queryControl(ctx context.Context, request *pb.QueryRequest) (*pb.QueryResponse, error) {
	if request.Kind < pb.QueryKind_APPLICATIONS || request.Kind > pb.QueryKind_METRICS {
		return nil, status.Error(codes.InvalidArgument, "query kind required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := app.Manager.LoadState(); err != nil {
		return nil, status.Error(codes.Internal, "application registry unreadable")
	}
	response := &pb.QueryResponse{}
	for _, item := range app.Manager.ListApplications() {
		if request.Application != "" && request.Application != item.ID && request.Application != item.Name {
			continue
		}
		model := &pb.Application{Id: item.ID, Name: item.Name, Status: item.Status, Port: int32(item.Port), Runtime: deploy.RuntimeNative}
		state, err := deploy.Load(item.Name)
		if err != nil && !os.IsNotExist(err) {
			return nil, status.Error(codes.Internal, "deployment state unreadable")
		}
		if state != nil {
			model.Runtime = state.Runtime
			model.Strategy = string(state.Mode)
			model.Version = int32(state.ActiveVersion)
			model.Status = deploy.DecideLifecycleContext(ctx, state).Status
			for _, group := range []map[string]*deploy.Instance{state.Slots, state.Replicas} {
				for _, instance := range group {
					if instance != nil {
						model.Instances = append(model.Instances, &pb.Instance{Slot: instance.Slot, ContainerId: instance.ContainerID, Endpoint: deploy.InstanceEndpoint(instance), Status: instance.Status, Version: int32(instance.Version), Pid: int32(instance.PID)})
					}
				}
			}
		}
		if state == nil && item.PID > 0 {
			model.Instances = append(model.Instances, &pb.Instance{Slot: "classic", Pid: int32(item.PID), Status: item.Status, Endpoint: fmt.Sprintf("127.0.0.1:%d", item.Port)})
		}
		response.Applications = append(response.Applications, model)
		if request.Kind == pb.QueryKind_METRICS && model.Runtime != deploy.RuntimeDocker {
			for _, instance := range model.Instances {
				if instance.Pid <= 0 || instance.Status != "running" {
					continue
				}
				memory, cpu, err := app.ProcessUsage(int(instance.Pid))
				if err != nil {
					return nil, status.Error(codes.Unavailable, "native process metrics unavailable")
				}
				response.Metrics = append(response.Metrics, &pb.Metrics{ApplicationId: item.ID, Pid: instance.Pid, CpuPercent: cpu, MemoryBytes: memory, State: model.Status})
			}
		}
		if request.Kind == pb.QueryKind_VERSIONS {
			versions, err := deploy.ListVersionsForDisplay(item.Name, deploy.DefaultRetention{Max: 5})
			if err != nil {
				return nil, status.Error(codes.Internal, phelixerr.Redact(err.Error()))
			}
			for _, v := range versions {
				response.Versions = append(response.Versions, &pb.Version{Number: int32(v.Version), Tag: v.Tag, Image: v.DockerImage, Current: v.IsCurrent, CreatedAt: v.BuiltAt.UTC().Format(time.RFC3339)})
			}
		}
		if request.Kind == pb.QueryKind_HISTORY {
			records, _, err := deploy.ReadRollbackHistory(item.Name)
			if err != nil {
				return nil, status.Error(codes.Internal, phelixerr.Redact(err.Error()))
			}
			for _, entry := range records {
				from, _ := strconv.Atoi(strings.TrimPrefix(entry.From, "v"))
				to, _ := strconv.Atoi(strings.TrimPrefix(entry.To, "v"))
				response.History = append(response.History, &pb.HistoryEntry{FromVersion: int32(from), ToVersion: int32(to), At: entry.Time.UTC().Format(time.RFC3339), Success: entry.Status == "success", Reason: phelixerr.Redact(entry.Reason)})
			}
		}
	}
	if request.Application != "" && len(response.Applications) == 0 {
		return nil, status.Error(codes.NotFound, "application not found")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return response, nil
}
