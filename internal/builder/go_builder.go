package builder

import (
	"os"
	"os/exec"

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

// Validate checks that scan describes a buildable Go project.
func (gb *GoBuilder) Validate(scan *ProjectScan) error {
	if scan == nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "validate : no project scan")
	}

	if !scan.Exists {
		return phelixerr.Newf(phelixerr.CodeNotFound, "project root does not exist: %s", scan.Root)
	}

	if scan.RootFile("go.mod") || scan.RootFile("main.go") {
		return nil
	}

	if scan.HasMainFile() {
		return nil
	}

	return phelixerr.New(phelixerr.CodeUnsupportedProject, "no valid Go project found: missing go.mod or main.go")
}

// observeCache records cache telemetry into config.Observe when present.
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
	scan := config.Scan

	if scan == nil {
		scan = NewProjectScan(config.ProjectRoot)
	}

	target, err := scan.GoBuildTarget()
	if err != nil {
		return err
	}

	args := []string{"build"}

	// Add extra arguments from user (e.g., -ldflags, -tags, etc.)
	if len(config.ExtraArgs) > 0 {
		args = append(args, config.ExtraArgs...)
	}

	args = append(args, "-o", config.OutputPath, target)

	cmd := exec.Command("go", args...)
	cmd.Dir = config.ProjectRoot

	output, err := cmd.CombinedOutput()
	if err != nil {
		return phelixerr.Wrapf(
			phelixerr.CodeBuildFailed,
			&ToolError{Tool: "go", Output: tailOutput(output), Err: err},
			"go build failed for %s (%s)",
			config.Name,
			config.Language,
		)
	}

	observeCache(config, buildreport.CacheUnknown, "")

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
		"-work",
		"-x",
	}
}
