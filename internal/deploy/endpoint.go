package deploy

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

func processEndpoint(process Process, port int) string {
	if source, ok := process.(interface{ Endpoint() string }); ok && source.Endpoint() != "" {
		return source.Endpoint()
	}
	return hostPort(port)
}

func InstanceEndpoint(instance *Instance) string {
	if instance == nil {
		return ""
	}
	if instance.Endpoint != "" {
		return instance.Endpoint
	}
	return hostPort(instance.Port)
}

func dockerEndpoint(ctx context.Context, run dockerRunner, id, network string) (string, int, error) {
	if os.Getenv("PHELIX_CONTROLLER_MODE") != "container" {
		port, err := dockerPublishedPort(ctx, run, id, dockerInternalPort)
		return hostPort(port), port, err
	}
	if network == "" {
		network = os.Getenv("PHELIX_CONTROLLER_NETWORK")
	}
	if network == "" {
		return "", 0, fmt.Errorf("container mode requires a shared Docker network")
	}
	format := "{{(index .NetworkSettings.Networks " + strconv.Quote(network) + ").IPAddress}}"
	output, err := run(ctx, "inspect", "--format", format, id)
	if err != nil {
		return "", 0, err
	}
	address := strings.TrimSpace(output)
	if net.ParseIP(address) == nil {
		return "", 0, fmt.Errorf("container has no address on Controller network %q", network)
	}
	return net.JoinHostPort(address, strconv.Itoa(dockerInternalPort)), dockerInternalPort, nil
}

func RefreshInstanceEndpoints(ctx context.Context, state *DeployState) error {
	if state == nil || state.Runtime != RuntimeDocker {
		return nil
	}
	changed := false
	for _, group := range []map[string]*Instance{state.Slots, state.Replicas} {
		for _, instance := range group {
			if instance == nil || instance.ContainerID == "" || !InstanceAlive(instance) {
				continue
			}
			endpoint, port, err := dockerEndpoint(ctx, dockerIdentityRunner, instance.ContainerID, state.Network)
			if err != nil {
				return err
			}
			if instance.Endpoint != endpoint || instance.Port != port {
				instance.Endpoint = endpoint
				instance.Port = port
				changed = true
			}
		}
	}
	if changed {
		return Store(state)
	}
	return nil
}
