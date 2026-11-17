package deploy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
)

func DockerLogFilePath(id string) string {
	return filepath.Join(server.DataDir(), "logs", "container_"+filepath.Base(id)+".log")
}

func openDockerLog(id string) (*os.File, error) {
	path := DockerLogFilePath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
}

func RecoverDockerLogs(ctx context.Context, state *DeployState) error {
	for _, group := range []map[string]*Instance{state.Slots, state.Replicas} {
		for _, instance := range group {
			if instance == nil || instance.ContainerID == "" || !InstanceAlive(instance) {
				continue
			}
			output, err := dockerIdentityRunner(ctx, "inspect", "--format", "{{json .Config.Env}}", instance.ContainerID)
			if err != nil {
				return err
			}
			var env []string
			if err := json.Unmarshal([]byte(output), &env); err != nil {
				return err
			}
			file, err := openDockerLog(instance.ContainerID)
			if err != nil {
				return err
			}
			err = logs.CaptureDockerLogs(instance.ContainerID, env, file)
			file.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}
