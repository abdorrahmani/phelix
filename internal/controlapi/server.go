package controlapi

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Service struct {
	pb.UnimplementedControlServer
	InfoResponse *pb.ControllerInfo
	Operations   *Operations
	Validate     func(*pb.CommandRequest) (bool, error)
	QueryHandler func(context.Context, *pb.QueryRequest) (*pb.QueryResponse, error)
}

func (s *Service) Health(ctx context.Context, request *pb.Empty) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{Ready: true}, nil
}
func (s *Service) Info(ctx context.Context, request *pb.Empty) (*pb.ControllerInfo, error) {
	return s.InfoResponse, nil
}
func (s *Service) Query(ctx context.Context, request *pb.QueryRequest) (*pb.QueryResponse, error) {
	if s.QueryHandler == nil {
		return nil, status.Error(codes.Unimplemented, "query unavailable")
	}
	return s.QueryHandler(ctx, request)
}
func (s *Service) Execute(ctx context.Context, request *pb.CommandRequest) (*pb.Operation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.Validate == nil {
		return nil, status.Error(codes.Unimplemented, "commands unavailable")
	}
	cancellable, err := s.Validate(request)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return s.Operations.Submit(request, cancellable)
}
func (s *Service) GetOperation(ctx context.Context, request *pb.OperationRequest) (*pb.Operation, error) {
	return s.Operations.Get(request.Id)
}
func (s *Service) CancelOperation(ctx context.Context, request *pb.OperationRequest) (*pb.Operation, error) {
	return s.Operations.Cancel(request.Id)
}
func (s *Service) WatchOperation(request *pb.WatchRequest, stream grpc.ServerStreamingServer[pb.Progress]) error {
	return s.Operations.Watch(stream.Context(), request.Id, request.AfterSequence, stream.Send)
}

type Server struct {
	grpc     *grpc.Server
	listener net.Listener
	socket   string
	done     chan struct{}
	once     sync.Once
}

func Listen(socket string, allowedUIDs []uint32, service *Service) (*Server, error) {
	if !filepath.IsAbs(socket) {
		return nil, fmt.Errorf("control socket must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace non-socket %s", socket)
		}
		conn, err := net.DialTimeout("unix", socket, time.Second)
		if err == nil {
			conn.Close()
			return nil, fmt.Errorf("control socket is already in use")
		}
		if err := os.Remove(socket); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	allowed := map[uint32]bool{}
	for _, uid := range allowedUIDs {
		allowed[uid] = true
	}
	server := grpc.NewServer(grpc.Creds(unixCredentials{allowed: allowed}), grpc.MaxRecvMsgSize(2<<20), grpc.MaxSendMsgSize(4<<20), grpc.MaxConcurrentStreams(128))
	pb.RegisterControlServer(server, service)
	s := &Server{grpc: server, listener: listener, socket: socket, done: make(chan struct{})}
	go func() { defer close(s.done); server.Serve(listener) }()
	return s, nil
}

func (s *Server) Close() {
	s.once.Do(func() { s.grpc.Stop(); s.listener.Close(); <-s.done })
}
