package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateScaleRange(t *testing.T) {
	dir := t.TempDir()
	cfg := []byte("name: api\ndeploy:\n  strategy: rolling\n  replicas: 2\n  autoscaling:\n    enabled: true\n    min_replicas: 2\n    max_replicas: 5\n")
	if err := os.WriteFile(filepath.Join(dir, "phelix.yaml"), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateScaleRange(dir, 3); err != nil {
		t.Fatalf("in-range count rejected: %v", err)
	}
	if err := validateScaleRange(dir, 1); err == nil {
		t.Fatal("count below min_replicas must be rejected")
	}
	if err := validateScaleRange(dir, 6); err == nil {
		t.Fatal("count above max_replicas must be rejected")
	}
}
