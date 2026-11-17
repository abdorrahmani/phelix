package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/controlapi"
	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
)

var testControllers = struct {
	sync.Mutex
	sockets map[string]string
}{sockets: map[string]string{}}

func prepareControlTest(t *testing.T, command *exec.Cmd) {
	t.Helper()
	values := map[string]string{}
	for _, entry := range command.Env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	directory := values["PHELIX_DATA_DIR"]
	if directory == "" {
		directory = filepath.Join(values["HOME"], ".phelix")
	}
	testControllers.Lock()
	defer testControllers.Unlock()
	socketPath := testControllers.sockets[directory]
	if socketPath == "" {
		socketDirectory, err := os.MkdirTemp("", "phelix-control-test-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(socketDirectory) })
		socketPath = filepath.Join(socketDirectory, "control.sock")
		log, err := os.Create(filepath.Join(socketDirectory, "controller.log"))
		if err != nil {
			t.Fatal(err)
		}
		controller := exec.Command(phelixBin(t), "controller")
		controller.Dir = command.Dir
		controller.Env = append(command.Env, "PHELIX_DATA_DIR="+directory, "PHELIX_CONTROL_SOCKET="+socketPath, "PHELIX_CONTROLLER_CONFIG=")
		controller.Stdout, controller.Stderr = log, log
		if err := controller.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- controller.Wait() }()
		t.Cleanup(func() {
			controller.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("test Controller shutdown: %v", err)
				}
			case <-time.After(45 * time.Second):
				controller.Process.Kill()
				<-done
				t.Error("test Controller did not shut down")
			}
			log.Close()
			testControllers.Lock()
			delete(testControllers.sockets, directory)
			testControllers.Unlock()
		})
		deadline := time.Now().Add(10 * time.Second)
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			connection, client, err := controlapi.Connect(ctx, socketPath)
			if err == nil {
				_, err = client.Health(ctx, &pb.Empty{})
				connection.Close()
			}
			cancel()
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				output, _ := os.ReadFile(log.Name())
				t.Fatalf("test Controller unavailable: %v\n%s", err, output)
			}
			time.Sleep(10 * time.Millisecond)
		}
		testControllers.sockets[directory] = socketPath
	}
	command.Env = append(command.Env, "PHELIX_DATA_DIR="+directory, "PHELIX_CONTROL_SOCKET="+socketPath, "PHELIX_CONTROLLER_CONFIG=")
}
