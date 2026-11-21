package controlapi

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStaleSocketRecoveryRequiresSafePermissions(t *testing.T) {
	for _, mode := range []os.FileMode{0600, 0666} {
		socket := filepath.Join(t.TempDir(), "control.sock")
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		listener.SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(socket, mode); err != nil {
			t.Fatal(err)
		}
		server, err := Listen(socket, []uint32{uint32(os.Getuid())}, &Service{})
		if mode == 0666 {
			if err == nil {
				server.Close()
				t.Fatal("unsafe stale socket was adopted")
			}
			if _, err := os.Stat(socket); err != nil {
				t.Fatal("rejected socket was removed", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		connection, _, err := Connect(ctx, socket)
		cancel()
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		connection.Close()
		server.Close()
	}
}
