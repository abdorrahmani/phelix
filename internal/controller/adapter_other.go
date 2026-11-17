//go:build !linux

package controller

import (
	"context"
	"fmt"
	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	"google.golang.org/protobuf/proto"
	"io"
	"net"
)

const AdapterArgument = "__phelix_controller_adapter"

func ReadAdapter() (net.Conn, *pb.CommandRequest, error) {
	return nil, nil, fmt.Errorf("Controller adapters currently require Linux")
}
func WriteMessage(io.Writer, proto.Message) error {
	return fmt.Errorf("Controller adapters currently require Linux")
}
func ExecuteAdapter(context.Context, *pb.CommandRequest, func(string, bool)) (int, error) {
	return 1, fmt.Errorf("Controller adapters currently require Linux")
}
