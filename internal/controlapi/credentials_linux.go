package controlapi

import (
	"context"
	"fmt"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/credentials"
)

type peerInfo struct{ UID uint32 }

func (p peerInfo) AuthType() string { return "unix-peer" }

type unixCredentials struct{ allowed map[uint32]bool }

func (c unixCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "unix-peer"}
}
func (c unixCredentials) Clone() credentials.TransportCredentials { return c }
func (c unixCredentials) OverrideServerName(string) error         { return nil }
func (c unixCredentials) ClientHandshake(ctx context.Context, authority string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	if _, ok := conn.(*net.UnixConn); !ok {
		return nil, nil, fmt.Errorf("local Control API requires Unix transport")
	}
	return conn, peerInfo{}, nil
}
func (c unixCredentials) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	socket, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, nil, fmt.Errorf("local Control API requires Unix transport")
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return nil, nil, err
	}
	var peer *unix.Ucred
	var peerErr error
	if err := raw.Control(func(fd uintptr) { peer, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return nil, nil, err
	}
	if peerErr != nil {
		return nil, nil, peerErr
	}
	if !c.allowed[peer.Uid] {
		return nil, nil, fmt.Errorf("local peer is not authorized")
	}
	return conn, peerInfo{UID: peer.Uid}, nil
}

func validateSocketOwner(info os.FileInfo, uid int) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != uint32(os.Getuid()) && stat.Uid != uint32(uid)) || info.Mode().Perm()&0007 != 0 {
		return fmt.Errorf("stale control socket has unsafe ownership or permissions")
	}
	return nil
}
