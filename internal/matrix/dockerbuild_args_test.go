package matrix

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// The per-combination build args must include the ecosystem toolchain arg the
// generated Dockerfiles parameterize on (ARG GO_VERSION / ARG RUST_VERSION).
// Without it, every combination of a matrix dockerize silently built with the
// Dockerfile's default toolchain while being tagged with the requested
// version.

// findBuildArgValue extracts the value of `--build-arg KEY=VALUE` from a docker
// command line (separate-argument form).
func findBuildArgValue(args []string, key string) (string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--build-arg" && strings.HasPrefix(args[i+1], key+"=") {
			return strings.TrimPrefix(args[i+1], key+"="), true
		}
	}
	return "", false
}

func TestDockerMatrixBuild_PassesToolchainBuildArgs(t *testing.T) {
	cases := []struct {
		name string
		comb Combination
		want map[string]string
		bad  string // arg that must NOT be passed
	}{
		{
			name: "go combination",
			comb: Combination{Lang: builder.Go, Version: "1.27", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
			want: map[string]string{"GO_VERSION": "1.27", "TARGETVERSION": "1.27", "TARGETPLATFORM": "linux/amd64", "TARGETOS": "linux", "TARGETARCH": "amd64"},
			bad:  "RUST_VERSION",
		},
		{
			name: "rust combination with variant",
			comb: Combination{Lang: builder.Rust, Version: "1.77", OS: "linux", Arch: "arm", Variant: "v7", Platform: "linux/arm/v7"},
			want: map[string]string{"RUST_VERSION": "1.77", "TARGETVERSION": "1.77", "TARGETPLATFORM": "linux/arm/v7", "TARGETOS": "linux", "TARGETARCH": "arm", "TARGETVARIANT": "v7"},
			bad:  "GO_VERSION",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buildArgs []string
			dmb := &DockerMatrixBuilder{
				ProjectRoot: t.TempDir(),
				AppName:     "app",
				CommandContext: func(ctx context.Context, name string, args ...string) *exec.Cmd {
					if len(args) > 0 && args[0] == "build" {
						buildArgs = args
					}
					if len(args) > 1 && args[0] == "image" {
						// Fake `docker image inspect --format {{.Id}}`.
						return exec.Command("sh", "-c", "echo sha256:"+strings.Repeat("a", 64))
					}
					return exec.Command("true")
				},
			}
			res := dmb.BuildDockerImage(context.Background(), tc.comb)
			if res.Status != "success" {
				t.Fatalf("build failed: %v", res.Error)
			}
			for key, want := range tc.want {
				got, ok := findBuildArgValue(buildArgs, key)
				if !ok || got != want {
					t.Fatalf("build arg %s = %q (present: %v), want %q; args: %v", key, got, ok, want, buildArgs)
				}
			}
			if _, ok := findBuildArgValue(buildArgs, tc.bad); ok {
				t.Fatalf("build arg %s must not be passed for this ecosystem; args: %v", tc.bad, buildArgs)
			}
		})
	}
}
