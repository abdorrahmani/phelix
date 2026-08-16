package docker

import (
	"os"
	"path/filepath"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// DefaultDockerignore returns a sensible .dockerignore content for Go/Rust projects.
// Excludes VCS data, local env files, build artifacts, logs, and IDE configs
// to prevent leaking secrets into the build context and to speed up builds.
func DefaultDockerignore() string {
	return `# Version control
.git
.gitignore

# Environment and secrets (never leak into build context)
.env
.env.*
*.env
*.env.*

# Build artifacts
app_*
*.exe
*.dll
*.so
*.dylib
target/
dist/
build/

# Logs
*.log
logs/

# IDE and editor files
.idea/
.vscode/
*.swp
*.swo
*~

# OS files
.DS_Store
Thumbs.db

# Phelix internals
.phelix/
.zcode/

# Documentation
README.md
LICENSE
docs/
`
}

// HasExistingDockerignore reports whether a .dockerignore already exists.
func HasExistingDockerignore(projectRoot string) bool {
	_, err := os.Stat(filepath.Join(projectRoot, ".dockerignore"))
	return err == nil
}

// WriteDockerignore creates a .dockerignore if none exists.
func WriteDockerignore(projectRoot string) error {
	if HasExistingDockerignore(projectRoot) {
		return nil
	}
	if err := os.WriteFile(filepath.Join(projectRoot, ".dockerignore"), []byte(DefaultDockerignore()), 0o644); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to write .dockerignore", err)
	}
	return nil
}
