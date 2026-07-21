package builder

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

// GoBuilder builds Go projects
type GoBuilder struct{}

// NewGoBuilder creates a new Go builder
func NewGoBuilder() *GoBuilder {
	return &GoBuilder{}
}

// Name returns "go"
func (gb *GoBuilder) Name() Language {
	return Go
}

// Validate checks if a valid Go project exists at the given root
func (gb *GoBuilder) Validate(projectRoot string) error {
	if _, err := os.Stat(projectRoot); os.IsNotExist(err) {
		return fmt.Errorf("project root does not exist: %s", projectRoot)
	}

	// Check for go.mod or main.go
	if _, err := os.Stat(filepath.Join(projectRoot, "go.mod")); err == nil {
		return nil
	}

	if _, err := os.Stat(filepath.Join(projectRoot, "main.go")); err == nil {
		return nil
	}

	// Try to find any main.go file
	found := false
	_ = filepath.WalkDir(projectRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if !d.IsDir() && d.Name() == "main.go" {
			found = true
			return io.EOF
		}
		return nil
	})

	if found {
		return nil
	}

	return fmt.Errorf("no valid Go project found: missing go.mod or main.go")
}

// Build performs a Go build
func (gb *GoBuilder) Build(config BuildConfig) error {
	mainFile, err := gb.findMainFile(config.ProjectRoot)
	if err != nil {
		return fmt.Errorf("failed to find main.go: %v", err)
	}

	relPath, _ := filepath.Rel(config.ProjectRoot, mainFile)

	// Build command arguments
	args := []string{"build"}

	// Add extra arguments from user (e.g., -ldflags, -tags, etc.)
	if len(config.ExtraArgs) > 0 {
		args = append(args, config.ExtraArgs...)
	}

	// Output path
	args = append(args, "-o", config.OutputPath, relPath)

	cmd := exec.Command("go", args...)
	cmd.Dir = config.ProjectRoot

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build failed: %v\nOutput:\n%s", err, string(output))
	}

	return nil
}

// GetBinaryPath returns the path to the built binary
func (gb *GoBuilder) GetBinaryPath(config BuildConfig) (string, error) {
	if _, err := os.Stat(config.OutputPath); err == nil {
		return config.OutputPath, nil
	}
	return "", fmt.Errorf("built binary not found at %s", config.OutputPath)
}

// GetBuildFlags returns Go build flags
func (gb *GoBuilder) GetBuildFlags() []string {
	return []string{
		"-a",
		"-asan",
		"-cover",
		"-covermode=mode",
		"-coverpkg=import/path",
		"-d=synchmem",
		"-flexint",
		"-gcflags",
		"-gccgoflags",
		"-installsuffix",
		"-ldflags",
		"-linkshared",
		"-msan",
		"-n",
		"-o",
		"-p",
		"-pkgdir",
		"-race",
		"-tags",
		"-toolexec",
		"-trimpath",
		"-v",
		"-work",
		"-x",
	}
}

// findMainFile searches for a main.go file in the project root
func (gb *GoBuilder) findMainFile(root string) (string, error) {
	var mainFile string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "main.go" {
			mainFile = path
			return io.EOF
		}
		return nil
	})
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("error searching for main.go: %w", err)
	}
	if mainFile == "" {
		return "", fmt.Errorf("no main.go file found in the project")
	}
	return mainFile, nil
}
