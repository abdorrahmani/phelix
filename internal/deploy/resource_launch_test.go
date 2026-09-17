package deploy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/abdorrahmani/phelix/internal/resources"
)

func TestDeploymentResourceSetupFailureDoesNotLaunch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PHELIX_CGROUP_ROOT", filepath.Join(dir, "not-a-cgroup"))
	marker := filepath.Join(dir, "started")
	bin := filepath.Join(dir, "candidate")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []resources.Config{{CPU: "500m"}, {Memory: "512Mi"}} {
		proc, port, err := launchInstance(context.Background(), bin, nil, cfg)
		if err == nil {
			t.Fatalf("expected resource setup error for %+v, got %v", cfg, err)
		}
		if proc != nil || port != 0 {
			t.Fatalf("setup failure returned process=%v port=%d", proc, port)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("candidate executed despite setup failure: %v", err)
	}
}
