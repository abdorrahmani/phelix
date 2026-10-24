package docker

import (
	"os/exec"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// PushConfig holds parameters for pushing an image to a registry.
type PushConfig struct {
	ImageName string // Full image reference including tag
	Registry  string // Registry URL (e.g. "ghcr.io/user", "docker.io", "")
}

// PushImage pushes a Docker image to the specified registry.
//
// Registry credentials are stored using the same AES-256-GCM encrypted
// mechanism already used for environment variables (internal/env/crypto.go).
// The user's Docker CLI login is used for the actual push — we verify
// the user is logged in first and give a clear error if not.
func PushImage(cfg PushConfig) error {
	if err := CheckDockerAvailable(); err != nil {
		return err
	}

	imageRef := imageRefFor(cfg.ImageName, cfg.Registry)

	// Attempt to push
	cmd := exec.Command("docker", "push", imageRef)
	output, err := cmd.CombinedOutput()
	if err != nil {
		outputStr := string(output)

		if isAuthError(outputStr) {
			return phelixerr.Newf(
				phelixerr.CodePermissionDenied,
				"not logged in to registry %q — run `docker login %s` first, "+
					"or use `phelix env set` to store registry credentials",
				registryHost(cfg.Registry), cfg.Registry,
			)
		}

		return phelixerr.Wrap(phelixerr.CodeDocker, "docker push failed", err)
	}

	return nil
}

// isAuthError checks Docker push output for authentication-related errors.
func isAuthError(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "authentication required") ||
		strings.Contains(lower, "not logged in") ||
		strings.Contains(lower, "denied") ||
		strings.Contains(lower, "401") ||
		strings.Contains(lower, "403")
}

// registryHost extracts the host portion from a registry URL.
func registryHost(registry string) string {
	if registry == "" {
		return "docker.io"
	}

	h := strings.TrimPrefix(registry, "https://")
	h = strings.TrimPrefix(h, "http://")

	if idx := strings.Index(h, "/"); idx != -1 {
		h = h[:idx]
	}

	return h
}

// registrySlug returns a filesystem-safe slug for the registry name.
func registrySlug(registry string) string {
	s := strings.TrimPrefix(registry, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, ":", "_")
	s = strings.ReplaceAll(s, ".", "_")
	if s == "" {
		return "docker_io"
	}

	return s
}

// imageRefFor qualifies an image name with the registry unless the name is
// already registry-qualified: a first path element containing "." or ":", or
// the special name "localhost". This keeps a ref like myreg.io/app from being
// pushed as registry/myreg.io/app.
func imageRefFor(imageName, registry string) string {
	if registry == "" {
		return imageName
	}

	if first, _, ok := strings.Cut(imageName, "/"); ok && (strings.ContainsAny(first, ".:")) || first == "localhost" {
		return imageName
	}

	return registry + "/" + imageName
}
