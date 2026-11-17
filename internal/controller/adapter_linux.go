package controller

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/server"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

const AdapterArgument = "__phelix_controller_adapter"

func WriteMessage(writer io.Writer, message proto.Message) error {
	raw, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	if len(raw) > 2<<20 {
		return fmt.Errorf("Controller request exceeds 2 MiB")
	}
	if err := binary.Write(writer, binary.BigEndian, uint32(len(raw))); err != nil {
		return err
	}
	_, err = writer.Write(raw)
	return err
}

func ReadMessage(reader io.Reader, message proto.Message) error {
	var length uint32
	if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
		return err
	}
	if length > 2<<20 {
		return fmt.Errorf("Controller request exceeds 2 MiB")
	}
	raw := make([]byte, length)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return err
	}
	return proto.Unmarshal(raw, message)
}

func ReadAdapter() (net.Conn, *pb.CommandRequest, error) {
	var owner unix.Stat_t
	if err := unix.Fstat(4, &owner); err != nil || owner.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, nil, fmt.Errorf("private Controller ownership descriptor invalid")
	}
	var expected unix.Stat_t
	if err := unix.Lstat(filepath.Join(server.DataDir(), "controller.lock"), &expected); err != nil || owner.Dev != expected.Dev || owner.Ino != expected.Ino {
		return nil, nil, fmt.Errorf("private Controller ownership descriptor does not match persistent state")
	}
	if err := unix.Flock(4, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, nil, fmt.Errorf("private Controller ownership descriptor is not the active ownership lease")
	}
	unix.CloseOnExec(4)
	file := os.NewFile(3, "controller-adapter")
	if file == nil {
		return nil, nil, fmt.Errorf("private Controller descriptor missing")
	}
	defer file.Close()
	conn, err := net.FileConn(file)
	if err != nil {
		return nil, nil, fmt.Errorf("private Controller descriptor invalid: %w", err)
	}
	socket, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close()
		return nil, nil, fmt.Errorf("private Controller transport invalid")
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	var peer *unix.Ucred
	var peerErr error
	err = raw.Control(func(fd uintptr) { peer, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	if err != nil || peerErr != nil || peer.Pid != int32(os.Getppid()) || peer.Uid != uint32(os.Getuid()) {
		conn.Close()
		return nil, nil, fmt.Errorf("private Controller parent authentication failed")
	}
	request := &pb.CommandRequest{}
	if err := ReadMessage(conn, request); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, request, nil
}

func (c *Controller) ExecuteAdapter(ctx context.Context, request *pb.CommandRequest, progress func(string, bool)) (int, error) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 1, err
	}
	parent := os.NewFile(uintptr(pair[0]), "controller-parent")
	defer parent.Close()
	child := os.NewFile(uintptr(pair[1]), "controller-child")
	defer child.Close()
	executable, err := os.Executable()
	if err != nil {
		return 1, err
	}
	command := exec.CommandContext(ctx, executable, AdapterArgument)
	command.ExtraFiles = []*os.File{child, c.owner.file}
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGINT}
	command.Stdin = bytes.NewReader(request.Input)
	command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
	stdout, err := command.StdoutPipe()
	if err != nil {
		return 1, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return 1, err
	}
	if err := command.Start(); err != nil {
		return 1, err
	}
	child.Close()
	if err := WriteMessage(parent, request); err != nil {
		command.Process.Signal(os.Interrupt)
		command.Wait()
		return 1, err
	}
	var streams sync.WaitGroup
	streams.Add(2)
	drain := func(reader io.Reader, isError bool) {
		defer streams.Done()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 8192), 2<<20)
		for scanner.Scan() {
			progress(scanner.Text()+"\n", isError)
		}
		if err := scanner.Err(); err != nil {
			progress("Controller output stream failed\n", true)
		}
	}
	go drain(stdout, false)
	go drain(stderr, true)
	response := &pb.Error{}
	responseErr := ReadMessage(parent, response)
	streams.Wait()
	waitErr := command.Wait()
	if ctx.Err() != nil {
		return 1, ctx.Err()
	}
	if responseErr != nil {
		return 1, fmt.Errorf("Controller adapter ended without a result: %w", responseErr)
	}
	if response.Code != "" {
		return command.ProcessState.ExitCode(), &adapterError{response}
	}
	if waitErr != nil {
		return command.ProcessState.ExitCode(), waitErr
	}
	return 0, nil
}

type adapterError struct{ record *pb.Error }

func (e *adapterError) Error() string { return e.record.Message }
func (e *adapterError) Unwrap() error {
	return phelixerr.New(phelixerr.Code(e.record.Code), e.record.Message)
}
func (e *adapterError) Display() string { return e.record.Display }
