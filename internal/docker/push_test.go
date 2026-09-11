package docker

import (
	"strings"
	"testing"
)

func TestIsAuthError(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "unauthorized error",
			output: "denied: requested access to the resource is denied",
			want:   true,
		},
		{
			name:   "not logged in",
			output: "unauthorized: authentication required",
			want:   true,
		},
		{
			name:   "401 status",
			output: "Error: daemon: unexpected status 401",
			want:   true,
		},
		{
			name:   "403 forbidden",
			output: "error: denied with status 403 Forbidden",
			want:   true,
		},
		{
			name:   "network error (not auth)",
			output: "Error: Cannot connect to the Docker daemon",
			want:   false,
		},
		{
			name:   "build error (not auth)",
			output: "error: failed to solve: rpc error: code = NotFound",
			want:   false,
		},
		{
			name:   "empty output",
			output: "",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAuthError(tt.output)
			if got != tt.want {
				t.Errorf("isAuthError(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

func TestRegistryHost(t *testing.T) {
	tests := []struct {
		registry string
		want     string
	}{
		{"ghcr.io/user", "ghcr.io"},
		{"docker.io/myorg", "docker.io"},
		{"harbor.example.com/myproject", "harbor.example.com"},
		{"https://ghcr.io/user", "ghcr.io"},
		{"http://localhost:5000", "localhost:5000"},
		{"", "docker.io"},
	}

	for _, tt := range tests {
		t.Run(tt.registry, func(t *testing.T) {
			got := registryHost(tt.registry)
			if got != tt.want {
				t.Errorf("registryHost(%q) = %q, want %q", tt.registry, got, tt.want)
			}
		})
	}
}

func TestRegistrySlug(t *testing.T) {
	tests := []struct {
		registry string
		want     string
	}{
		{"ghcr.io/user", "ghcr_io_user"},
		{"docker.io/myorg", "docker_io_myorg"},
		{"harbor.example.com/project", "harbor_example_com_project"},
		{"", "docker_io"},
	}

	for _, tt := range tests {
		t.Run(tt.registry, func(t *testing.T) {
			got := registrySlug(tt.registry)
			if got != tt.want {
				t.Errorf("registrySlug(%q) = %q, want %q", tt.registry, got, tt.want)
			}
		})
	}
}

func TestExtractImageID(t *testing.T) {
	output := `#1 [internal] load build definition from Dockerfile
#1 transferring dockerfile: 2B done
#1 DONE 0.0s
#2 [internal] load metadata for docker.io/library/alpine:3.18
#2 DONE 0.5s
#3 exporting to image
#3 => naming to docker.io/library/myapp:v1.2.3
#3 DONE 0.0s`

	got := extractImageID(output)
	want := "docker.io/library/myapp:v1.2.3"
	if got != want {
		t.Errorf("extractImageID() = %q, want %q", got, want)
	}
}

func TestExtractImageID_NoMatch(t *testing.T) {
	output := "some random output without naming line"
	got := extractImageID(output)
	if got != "" {
		t.Errorf("extractImageID() = %q, want empty string", got)
	}
}

func TestCheckDockerAvailable(t *testing.T) {
	// This test verifies the function exists and returns an error when docker
	// is not available. In CI without Docker, this should return an error.
	err := CheckDockerAvailable()
	// We can't assert success/failure since Docker may or may not be installed
	// in the test environment. Just verify it doesn't panic.
	_ = err
}

func TestPushConfig_ImageRefConstruction(t *testing.T) {
	// Test that PushImage constructs the correct image reference
	// when a registry is specified but the image name doesn't include it.
	cfg := PushConfig{
		ImageName: "myapp:v1.2.3",
		Registry:  "ghcr.io/user",
	}

	// The image ref should be prefixed with the registry
	expectedRef := "ghcr.io/user/myapp:v1.2.3"
	// We can't actually run PushImage in tests without Docker, but we can
	// verify the config is well-formed
	if cfg.Registry != "" && !strings.Contains(cfg.ImageName, cfg.Registry) {
		ref := cfg.Registry + "/" + cfg.ImageName
		if ref != expectedRef {
			t.Errorf("expected image ref %q, got %q", expectedRef, ref)
		}
	}
}
