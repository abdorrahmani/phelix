package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/resources"
)

func TestClassicResourceSetupFailureDoesNotLaunch(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	bin := filepath.Join(dir, "app_resource-test")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PHELIX_CGROUP_ROOT", filepath.Join(dir, "not-a-cgroup"))
	info := &AppInfo{ID: "resource-test", Name: "resource-test", Directory: dir, Resources: resources.Config{CPU: "500m"}}
	m := &AppManager{Apps: map[string]*AppInfo{info.ID: info}}
	err := m.startApplicationProcess(info.ID, info.Name, 3000, filepath.Join(dir, "app.log"))
	if err == nil || !strings.Contains(err.Error(), "resource") {
		t.Fatalf("expected resource setup error, got %v", err)
	}
	if info.PID != 0 || info.Cmd != nil {
		t.Fatalf("failed setup recorded running process: %+v", info)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("application executed despite setup failure: %v", err)
	}
}
