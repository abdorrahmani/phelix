package builder

import (
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/abdorrahmani/phelix/internal/buildreport"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
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
		return phelixerr.Newf(phelixerr.CodeNotFound, "project root does not exist: %s", projectRoot)
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

	return phelixerr.New(phelixerr.CodeUnsupportedProject, "no valid Go project found: missing go.mod or main.go")
}

// observeCache records cache telemetry into config.Observe when present.
// Best-effort by design: a nil observation or unknown status simply leaves
// the report's cache section unknown.
func observeCache(config BuildConfig, status buildreport.CacheStatus, source string) {
	if config.Observe == nil {
		return
	}
	if status != buildreport.CacheCold && status != buildreport.CacheHit {
		status = buildreport.CacheUnknown
	}
	config.Observe.CacheStatus = status
	config.Observe.CacheSource = source
}

// Build performs a Go build
func (gb *GoBuilder) Build(config BuildConfig) error {
	mainFile, err := gb.findMainFile(config.ProjectRoot)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeUnsupportedProject, err, "failed to find main.go")
	}

	relPath, _ := filepath.Rel(config.ProjectRoot, mainFile)

	// Build command arguments.
	//
	// `-v` makes `go build` print every package being compiled, which is how
	// the build report distinguishes a cache COLD build from a cache HIT (all
	// packages up to date). It does not change what is compiled or linked.
	args := []string{"build", "-v"}

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
		// The compiler output is not embedded in the message. It is kept as a
		// bounded tail on the ToolError so the CLI error reporter can classify
		// known go.mod failures and show raw diagnostics; the underlying
		// *exec.ExitError (and its exit code) is preserved through the wraps so
		// errors.As / errors.Is still reach it.
		return phelixerr.Wrapf(
			phelixerr.CodeBuildFailed,
			&ToolError{Tool: "go", Output: tailOutput(output), Err: err},
			"go build failed for %s (%s)",
			config.Name,
			config.Language,
		)
	}

	observeCache(config, GoCacheStatusFromOutput(string(output)), buildreport.CacheSourceGoBuild)

	return nil
}

// GetBinaryPath returns the path to the built binary
func (gb *GoBuilder) GetBinaryPath(config BuildConfig) (string, error) {
	if _, err := os.Stat(config.OutputPath); err == nil {
		return config.OutputPath, nil
	}
	return "", phelixerr.Newf(phelixerr.CodeNotFound, "built binary not found at %s", config.OutputPath)
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
		return "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "error searching for main.go")
	}
	if mainFile == "" {
		return "", phelixerr.New(phelixerr.CodeUnsupportedProject, "no main.go file found in the project")
	}
	return mainFile, nil
}
