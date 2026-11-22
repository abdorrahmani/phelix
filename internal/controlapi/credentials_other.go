//go:build !linux

package controlapi

import (
	"context"
	"fmt"
	"google.golang.org/grpc/credentials"
	"net"
	"os"
)

type unixCredentials struct{ allowed map[uint32]bool }

func (c unixCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "unix-peer"}
}
func (c unixCredentials) Clone() credentials.TransportCredentials { return c }
func (c unixCredentials) OverrideServerName(string) error         { return nil }
func (c unixCredentials) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, fmt.Errorf("local peer authentication currently requires Linux")
}
func (c unixCredentials) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, fmt.Errorf("local peer authentication currently requires Linux")
}

func validateSocketOwner(info os.FileInfo, uid int) error {
	return fmt.Errorf("socket ownership validation requires Linux")
}
