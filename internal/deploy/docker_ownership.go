package deploy

import (
	"context"
	"encoding/json"
	"os"

	"github.com/abdorrahmani/phelix/internal/app"
)

const DockerLabelController = "phelix.controller"
const DockerLabelAppID = "phelix.app-id"

type instanceSlotKey struct{}

func launchSlot(ctx context.Context, slot string) context.Context {
	return context.WithValue(ctx, instanceSlotKey{}, slot)
}

func ownedDockerContainer(ctx context.Context, run dockerRunner, id string) (bool, error) {
	owner := os.Getenv("PHELIX_CONTROLLER_ID")
	if owner == "" {
		return true, nil
	}
	output, err := run(ctx, "inspect", "--format", "{{json .Config.Labels}}", id)
	if err != nil {
		return false, err
	}
	var labels map[string]string
	if json.Unmarshal([]byte(output), &labels) != nil || labels[DockerLabelManaged] != "true" {
		return false, nil
	}
	if labels[DockerLabelController] != "" {
		return labels[DockerLabelController] == owner, nil
	}
	name := labels[DockerLabelApp]
	if _, registered := app.AppDirectories()[name]; !registered {
		return false, nil
	}
	state, err := Load(name)
	if err != nil {
		return false, err
	}
	for _, known := range KnownContainerIDs(state) {
		if known == id {
			return true, nil
		}
	}
	return false, nil
}
