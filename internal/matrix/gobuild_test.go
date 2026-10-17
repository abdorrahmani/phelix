package matrix

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/builder"
)

func TestGoVersionProvides(t *testing.T) {
	cases := []struct {
		requested, host string
		want            bool
	}{
		{"1.22", "go1.22", true},
		{"1.22", "go1.22.4", true},   // minor request matches any patch
		{"1.22.4", "go1.22.4", true}, // exact patch match
		{"1.22.4", "go1.22.5", false},
		{"1.22", "go1.24.0", false},
		{"1.22.4", "go1.22", false}, // patch request needs the exact patch
		{"v1.22", "go1.22.4", true}, // prefixes normalized
		{"1.22", "", false},
		{"1.22", "garbage", false},
		{"1.27.2", "go1.27.2-X:nodwarf5", true},
		{"1.27", "go1.27.2-X:nodwarf5", true},
		{"1.27.1", "go1.27.2-X:nodwarf5", false},
		{"1.28", "go1.27.2-X:nodwarf5", false},
	}
	for _, tc := range cases {
		if got := goVersionProvides(tc.requested, tc.host); got != tc.want {
			t.Errorf("goVersionProvides(%q, %q) = %v, want %v", tc.requested, tc.host, got, tc.want)
		}
	}
}

func writeGoProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module t\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A single-version matrix build whose requested version differs from the host
// toolchain must fall back to the version-pinned Docker image — the native
// path would compile with the host version while labeling the artifact with
// the requested one.
func TestGoMatrixBuild_VersionMismatchFallsBackToDocker(t *testing.T) {
	dir := writeGoProject(t)

	var dockerImages []string
	sawNativeBuild := false
	gb := &GoMatrixBuilder{
		ProjectRoot: dir,
		AppName:     "t",
		LookPath:    func(string) (string, error) { return "/usr/bin/go", nil },
		CommandContext: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			switch {
			case name == "go" && len(args) > 0 && args[0] == "env":
				return exec.Command("sh", "-c", "echo go1.24.0")
			case name == "go" && len(args) > 0 && args[0] == "build":
				sawNativeBuild = true
			case name == "docker":
				for _, a := range args {
					if strings.HasPrefix(a, "golang:") {
						dockerImages = append(dockerImages, a)
					}
				}
			}
			return exec.Command("true")
		},
	}

	c := Combination{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	gb.Build(context.Background(), c)
	if sawNativeBuild {
		t.Fatal("host toolchain (1.24.0) must not natively build a 1.22 combination")
	}
	if len(dockerImages) == 0 || dockerImages[0] != "golang:1.22" {
		t.Fatalf("expected the version-pinned golang:1.22 image, got %v", dockerImages)
	}
}

// The Docker path must announce its stages, so the progress cell names what
// the wait is (a first-time image pull) instead of sitting at "starting 0%".
// This also guards the named-progressReporter type in WithBuildProgress: a
// bare func literal stored there fails the assertion in ReportBuildProgress
// and silently drops every stage.
func TestGoMatrixBuild_DockerPathReportsStages(t *testing.T) {
	dir := writeGoProject(t)

	gb := &GoMatrixBuilder{
		ProjectRoot: dir,
		AppName:     "t",
		LookPath:    func(string) (string, error) { return "/usr/bin/go", nil },
		CommandContext: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if name == "go" && len(args) > 0 && args[0] == "env" {
				return exec.Command("sh", "-c", "echo go1.24.0")
			}
			return exec.Command("true")
		},
	}

	c := Combination{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	var stages []string
	ctx := WithBuildProgress(context.Background(), func(key, stage string, current, total int64) {
		stages = append(stages, stage)
	})
	gb.Build(ctx, c)

	if len(stages) == 0 {
		t.Fatal("no build stages reported — ReportBuildProgress cannot see the reporter")
	}
	if !strings.HasPrefix(stages[0], "pulling golang:1.22") {
		t.Errorf("first stage = %q, want the image pull announced", stages[0])
	}
}

// When the host toolchain IS the requested version, the fast native path is
// used (no Docker).
func TestGoMatrixBuild_HostVersionMatchUsesNative(t *testing.T) {
	dir := writeGoProject(t)

	sawDocker := false
	gb := &GoMatrixBuilder{
		ProjectRoot: dir,
		AppName:     "t",
		LookPath:    func(string) (string, error) { return "/usr/bin/go", nil },
		CommandContext: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if name == "docker" {
				sawDocker = true
			}
			if name == "go" && len(args) > 0 && args[0] == "env" {
				return exec.Command("sh", "-c", "echo go1.22.7")
			}
			return exec.Command("true")
		},
	}

	c := Combination{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	gb.Build(context.Background(), c)
	if sawDocker {
		t.Fatal("host toolchain 1.22.7 must natively build a 1.22 combination — no Docker")
	}
}
