package deploy

import (
	"context"
	"fmt"
	"strings"
)

func RemoveStoppedContainers(ctx context.Context, state *DeployState) error {
	for _, id := range KnownContainerIDs(state) {
		owned, err := ownedDockerContainer(ctx, execDockerRunner, id)
		if isDockerNotRunning(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !owned {
			continue
		}
		output, err := execDockerRunner(ctx, "inspect", "--format", "{{.State.Running}}", id)
		if isDockerNotRunning(err) {
			continue
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(output) != "false" {
			return fmt.Errorf("refusing to remove a running container")
		}
		if _, err := execDockerRunner(ctx, "rm", id); err != nil && !isDockerNotRunning(err) {
			return err
		}
	}
	return nil
}
