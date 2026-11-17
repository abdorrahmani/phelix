package controller

import (
	"context"
	"errors"
	"testing"
)

func TestContainerStartupRequiresPersistentMountsAndSharedNetwork(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", "/state")
	cfg := Config{Mode: "container", Network: "backend", Socket: "/run/phelix/control.sock", Sources: []SourceMount{{Host: "/host/app", Controller: "/workspace"}}}
	valid := `[{"NetworkSettings":{"Networks":{"backend":{}}},"Mounts":[{"Destination":"/state","Type":"volume","RW":true},{"Destination":"/run/phelix","Type":"bind","RW":true},{"Destination":"/workspace","Type":"bind","RW":false}]}]`
	for _, test := range []struct {
		name     string
		metadata string
		failure  bool
	}{
		{"persistent", valid, false},
		{"writable-layer", `[{"NetworkSettings":{"Networks":{"backend":{}}}}]`, true},
		{"wrong-network", `[{"NetworkSettings":{"Networks":{"other":{}}}}]`, true},
		{"invalid-metadata", "invalid", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateDockerEnvironment(context.Background(), cfg, func(context.Context, string) ([]byte, error) { return []byte(test.metadata), nil })
			if (err != nil) != test.failure {
				t.Fatal(err)
			}
		})
	}
	if err := validateDockerEnvironment(context.Background(), cfg, func(context.Context, string) ([]byte, error) { return nil, errors.New("daemon unavailable") }); err == nil {
		t.Fatal("Docker socket failure was hidden")
	}
}
