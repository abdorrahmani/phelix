package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestControllerEndpoints(t *testing.T) {
	if got := InstanceEndpoint(&Instance{Port: 1234}); got != "127.0.0.1:1234" {
		t.Fatal(got)
	}
	if got := InstanceEndpoint(&Instance{Port: 8080, Endpoint: "172.22.0.2:8080"}); got != "172.22.0.2:8080" {
		t.Fatal(got)
	}
	t.Setenv("PHELIX_CONTROLLER_MODE", "container")
	t.Setenv("PHELIX_CONTROLLER_NETWORK", "production")
	calls := [][]string{}
	run := func(ctx context.Context, args ...string) (string, error) {
		calls = append(calls, args)
		if args[0] == "run" {
			return "container", nil
		}
		if args[0] == "inspect" {
			return "172.22.0.2", nil
		}
		return "", nil
	}
	process, port, err := newDockerLauncher("app", "blue", "production", run)(context.Background(), "app:v1", nil)
	if err != nil || port != 8080 || processEndpoint(process, port) != "172.22.0.2:8080" {
		t.Fatal(port, err)
	}
	for _, call := range calls {
		for _, arg := range call {
			if arg == "-p" || arg == "--network-alias" {
				t.Fatal("bridge instance published or aliased", call)
			}
		}
	}
}

func TestControllerRollingMembershipUniqueEndpoints(t *testing.T) {
	replicas := map[string]*Instance{"0": {Slot: "0", Port: 8080, Endpoint: "172.22.0.2:8080", Status: "running"}, "1": {Slot: "1", Port: 8080, Endpoint: "172.22.0.3:8080", Status: "running"}}
	primary, backends := membershipAfterReplace(replicas, "0", 8080, "172.22.0.4:8080")
	if primary.Host != "172.22.0.4:8080" || len(backends) != 1 || backends[0].Host != "172.22.0.3:8080" {
		t.Fatal(primary, backends)
	}
	if replicas["0"].Endpoint != "172.22.0.2:8080" {
		t.Fatal("membership preview mutated stable replica")
	}
}

func TestControllerOwnershipProtectsForeignContainers(t *testing.T) {
	t.Setenv("PHELIX_CONTROLLER_ID", "owner")
	removed := false
	run := func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "ps" {
			return "foreign", nil
		}
		if args[0] == "inspect" {
			return `{"phelix.managed":"true","phelix.controller":"other"}`, nil
		}
		if args[0] == "rm" {
			removed = true
		}
		return "", nil
	}
	ids, err := reconcileOrphanContainers(context.Background(), "app", nil, run)
	if err != nil || len(ids) != 0 || removed {
		t.Fatal(ids, err, removed)
	}
	withIdentityRunner(t, run)
	report := stopByContainerID(context.Background(), "foreign", 0, 0)
	if !report.Exited || removed {
		t.Fatal(report)
	}
}

func TestControllerEndpointFailureDoesNotRouteCandidate(t *testing.T) {
	t.Setenv("PHELIX_CONTROLLER_MODE", "container")
	t.Setenv("PHELIX_CONTROLLER_NETWORK", "production")
	killed := false
	run := func(ctx context.Context, args ...string) (string, error) {
		switch args[0] {
		case "run":
			return "container", nil
		case "inspect":
			return "", errors.New("daemon unavailable")
		case "kill":
			killed = true
		}
		return "", nil
	}
	process, _, err := newDockerLauncher("app", "blue", "production", run)(context.Background(), "app:v1", nil)
	if err == nil || process != nil || !killed {
		t.Fatal("unroutable candidate leaked", err)
	}
	if _, _, err := dockerEndpoint(context.Background(), func(context.Context, ...string) (string, error) { return "<no value>", nil }, "container", "production"); err == nil || !strings.Contains(err.Error(), "no address") {
		t.Fatal(err)
	}
}

func TestEndpointRecoveryRejectsUnreachableNetworksAndCancellation(t *testing.T) {
	t.Setenv("PHELIX_CONTROLLER_MODE", "container")
	t.Setenv("PHELIX_CONTROLLER_NETWORK", "shared")
	run := func(context.Context, ...string) (string, error) {
		t.Fatal("Docker inspection ran for an invalid endpoint request")
		return "", nil
	}
	if _, _, err := dockerEndpoint(context.Background(), run, "container", "other"); err == nil {
		t.Fatal("an endpoint on an unreachable network was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RefreshInstanceEndpoints(ctx, &DeployState{Runtime: RuntimeDocker}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, _, err := dockerEndpoint(ctx, run, "container", "shared"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
