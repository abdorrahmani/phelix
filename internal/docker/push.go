package docker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/abdorrahmani/phelix/internal/env"
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
	// Verify docker is available
	if err := CheckDockerAvailable(); err != nil {
		return err
	}

	// If a custom registry is specified, ensure the image is tagged for it
	imageRef := cfg.ImageName
	if cfg.Registry != "" {
		// Ensure the image reference includes the registry prefix
		if !strings.Contains(cfg.ImageName, "/") || !strings.ContainsAny(cfg.ImageName, ".:/") {
			imageRef = cfg.Registry + "/" + cfg.ImageName
		}
	}

	// Attempt to push
	cmd := exec.Command("docker", "push", imageRef)
	output, err := cmd.CombinedOutput()
	if err != nil {
		outputStr := string(output)

		// Distinguish "not logged in" from other push failures
		if isAuthError(outputStr) {
			return fmt.Errorf(
				"not logged in to registry %q — run `docker login %s` first, "+
					"or use `phelix env set` to store registry credentials",
				registryHost(cfg.Registry), cfg.Registry,
			)
		}

		return fmt.Errorf("docker push failed: %w\nOutput:\n%s", err, outputStr)
	}

	return nil
}

// StoreRegistryCredentials encrypts and stores registry credentials using
// the same AES-256-GCM mechanism as env vars.
func StoreRegistryCredentials(registry, username, passwordOrToken string) error {
	masterKey, err := env.GetMasterKey()
	if err != nil {
		return fmt.Errorf("failed to get master key for credential storage: %w", err)
	}

	// Store username
	encUser, err := env.EncryptData(username, masterKey)
	if err != nil {
		return fmt.Errorf("failed to encrypt username: %w", err)
	}
	// Store password/token
	encPass, err := env.EncryptData(passwordOrToken, masterKey)
	if err != nil {
		return fmt.Errorf("failed to encrypt password: %w", err)
	}

	// Write to ~/.phelix/registry/<registry>.enc
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	regDir := filepath.Join(home, ".phelix", "registry")
	if err := os.MkdirAll(regDir, 0o700); err != nil {
		return err
	}

	credFile := filepath.Join(regDir, registrySlug(registry)+".enc")
	content := fmt.Sprintf("username:%s\npassword:%s", encUser, encPass)
	return os.WriteFile(credFile, []byte(content), 0o600)
}

// LoadRegistryCredentials retrieves and decrypts stored registry credentials.
func LoadRegistryCredentials(registry string) (username, password string, err error) {
	masterKey, err := env.GetMasterKey()
	if err != nil {
		return "", "", fmt.Errorf("failed to get master key: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}

	credFile := filepath.Join(home, ".phelix", "registry", registrySlug(registry)+".enc")
	data, err := os.ReadFile(credFile)
	if os.IsNotExist(err) {
		return "", "", fmt.Errorf("no stored credentials for registry %q", registry)
	}
	if err != nil {
		return "", "", err
	}

	// Parse the stored format: "username:<enc>\npassword:<enc>"
	lines := strings.SplitN(string(data), "\n", 2)
	if len(lines) < 2 {
		return "", "", fmt.Errorf("corrupted credential file for %q", registry)
	}

	for _, line := range lines {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "username":
			username, err = env.DecryptData(parts[1], masterKey)
			if err != nil {
				return "", "", fmt.Errorf("failed to decrypt username: %w", err)
			}
		case "password":
			password, err = env.DecryptData(parts[1], masterKey)
			if err != nil {
				return "", "", fmt.Errorf("failed to decrypt password: %w", err)
			}
		}
	}

	return username, password, nil
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
	// Strip protocol if present
	h := strings.TrimPrefix(registry, "https://")
	h = strings.TrimPrefix(h, "http://")
	// Take only the host part (before any path)
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
