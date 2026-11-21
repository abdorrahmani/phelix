package controlapi

import (
	"context"
	"fmt"
	"net"

	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	"google.golang.org/grpc"
)

func Connect(ctx context.Context, socket string) (*grpc.ClientConn, pb.ControlClient, error) {
	connection, err := grpc.NewClient("passthrough:///"+socket,
		grpc.WithTransportCredentials(unixCredentials{}),
		grpc.WithContextDialer(func(ctx context.Context, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4<<20)))
	if err != nil {
		return nil, nil, err
	}
	client := pb.NewControlClient(connection)
	if response, err := client.Health(ctx, &pb.Empty{}); err != nil || !response.GetReady() {
		if err == nil {
			err = fmt.Errorf("Controller is not ready")
		}
		connection.Close()
		return nil, nil, fmt.Errorf("cannot connect to Phelix Controller at %s; start 'phelix controller' or set PHELIX_CONTROL_SOCKET: %w", socket, err)
	}
	return connection, client, nil
}
