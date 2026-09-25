package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// withTempHome isolates on-disk state (versions.json, env snapshots) so the
// source tests never touch a real ~/.phelix.
func withTempHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PHELIX_DATA_DIR", dir)
}

func TestDockerBuildSource_RecordsVersionAndReturnsImageRef(t *testing.T) {
	withTempHome(t)

	var gotApp string
	var gotVer int
	src := &DockerBuildSource{
		AppName: "billing",
		AppID:   "app-1",
		Tag:     "hotfix",
		BuildFn: func(_ context.Context, app string, ver int) (string, error) {
			gotApp, gotVer = app, ver
			return fmt.Sprintf("billing:v%d", ver), nil
		},
	}

	imageRef, envPath, err := src.Build(context.Background())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if gotApp != "billing" {
		t.Errorf("builder got app %q", gotApp)
	}
	if gotVer != 1 {
		t.Errorf("first build should be v1, got v%d", gotVer)
	}
	if imageRef != "billing:v1" {
		t.Errorf("image ref = %q, want billing:v1", imageRef)
	}
	if src.TargetVersion() != 1 {
		t.Errorf("target version = %d, want 1", src.TargetVersion())
	}
	// Env snapshot must be paired for rollback (even with no env configured, an
	// empty store snapshot is written).
	if envPath == "" {
		t.Error("expected a paired env snapshot path")
	}
	if _, err := os.Stat(envPath); err != nil {
		t.Errorf("env snapshot not written: %v", err)
	}

	// The version must be recorded with is_current=false (two-phase promotion).
	vf, err := LoadVersions("billing")
	if err != nil {
		t.Fatalf("load versions: %v", err)
	}
	if len(vf.Versions) != 1 {
		t.Fatalf("expected 1 version, got %d", len(vf.Versions))
	}
	if vf.Versions[0].IsCurrent {
		t.Error("new version must not be current before deploy succeeds")
	}
	if vf.Versions[0].DockerImage != "billing:v1" {
		t.Errorf("recorded docker image = %q", vf.Versions[0].DockerImage)
	}
}

func TestDockerBuildSource_BuildFailurePropagates(t *testing.T) {
	withTempHome(t)
	src := &DockerBuildSource{
		AppName: "app",
		BuildFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", errors.New("dockerfile broken")
		},
	}
	_, _, err := src.Build(context.Background())
	if err == nil {
		t.Fatal("expected build error")
	}
	if phelixerr.CodeOf(err) != phelixerr.CodeBuildFailed {
		t.Fatalf("code = %v, want BUILD_FAILED", phelixerr.CodeOf(err))
	}
	// A failed build must not record a version.
	vf, _ := LoadVersions("app")
	if vf != nil && len(vf.Versions) != 0 {
		t.Errorf("failed build recorded a version: %+v", vf.Versions)
	}
}

func TestDockerBuildSource_NoBuildFn(t *testing.T) {
	withTempHome(t)
	src := &DockerBuildSource{AppName: "app"}
	if _, _, err := src.Build(context.Background()); phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
		t.Fatalf("expected INVALID_ARGUMENT, got %v", err)
	}
}

func TestDockerVersionSource_ResolvesRecordedImage(t *testing.T) {
	withTempHome(t)

	// Record two docker versions so we can roll back to a specific one.
	if _, err := RecordDockerBuild("api", "api:v1", "", "", DefaultRetention{Max: 5}, nil); err != nil {
		t.Fatalf("record v1: %v", err)
	}
	if _, err := RecordDockerBuild("api", "api:v2", "", "", DefaultRetention{Max: 5}, nil); err != nil {
		t.Fatalf("record v2: %v", err)
	}

	src := &DockerVersionSource{AppName: "api", AppID: "app-api", Version: 1}
	imageRef, _, err := src.Build(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if imageRef != "api:v1" {
		t.Errorf("image ref = %q, want api:v1", imageRef)
	}
	if src.TargetVersion() != 1 {
		t.Errorf("target = %d, want 1", src.TargetVersion())
	}
}

func TestDockerImageForVersion_NativeVersionHasNoImage(t *testing.T) {
	withTempHome(t)

	// A native (binary) version has no DockerImage — resolving it as a docker
	// image must fail clearly rather than return "".
	appDir := filepath.Join(os.Getenv("PHELIX_DATA_DIR"), "apps", "svc", "builds", "v1")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordFreshBuildForTest(t, "svc"); err != nil {
		t.Fatalf("record native: %v", err)
	}
	_, err := DockerImageForVersion("svc", 1)
	if phelixerr.CodeOf(err) != phelixerr.CodeVersionNotFound {
		t.Fatalf("expected VERSION_NOT_FOUND for native version, got %v", err)
	}
}

// RecordFreshBuildForTest records a native version by writing a dummy binary so
// RecordFreshBuild's copy step succeeds, exercising the "native version, no
// docker image" branch of DockerImageForVersion.
func RecordFreshBuildForTest(t *testing.T, app string) (*RecordResult, error) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "dummy")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return RecordFreshBuild(app, "app-"+app, bin, "", "", nil, DefaultRetention{Max: 5}, nil)
}
