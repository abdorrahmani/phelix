package builder

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/buildreport"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// BuildManager orchestrates the build process
type BuildManager struct {
	factory *BuilderFactory
}

// NewBuildManager creates a new build manager
func NewBuildManager() *BuildManager {
	return &BuildManager{
		factory: NewBuilderFactory(),
	}
}

// DetectLanguage detects the programming language of a project
func (bm *BuildManager) DetectLanguage(projectRoot string) Language {
	if _, err := os.Stat(filepath.Join(projectRoot, "Cargo.toml")); err == nil {
		return Rust
	}

	if _, err := os.Stat(filepath.Join(projectRoot, "main.go")); err == nil {
		return Go
	}

	if _, err := os.Stat(filepath.Join(projectRoot, "go.mod")); err == nil {
		return Go
	}

	hasGoFile := false
	_ = filepath.WalkDir(projectRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || hasGoFile {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()

		buf := make([]byte, 1024)
		n, _ := f.Read(buf)
		if n > 0 && strings.Contains(string(buf[:n]), "package main") {
			hasGoFile = true
		}
		return nil
	})

	if hasGoFile {
		return Go
	}

	return Language("unknown")
}

// BuildContext wraps the BuildConfig and adds additional context
type BuildContext struct {
	Config  *BuildConfig
	Lang    Language
	Builder BuilderInterface
}

// PrepareBuild validates and prepares the build context
func (bm *BuildManager) PrepareBuild(
	appID string,
	appName string,
	projectRoot string,
	outputPath string,
	detectLang Language,
	extraArgs []string,
	noUpload bool,
) (*BuildContext, error) {
	lang := detectLang
	if !lang.IsSupported() {
		lang = bm.DetectLanguage(projectRoot)
	}

	if !lang.IsSupported() {
		return nil, phelixerr.Newf(phelixerr.CodeUnsupportedProject, "unsupported or unknown project language: %s", lang)
	}

	builder, err := bm.factory.GetBuilder(lang)
	if err != nil {
		return nil, err
	}

	if err := builder.Validate(projectRoot); err != nil {
		return nil, err
	}

	config := &BuildConfig{
		ID:          appID,
		Name:        appName,
		ProjectRoot: projectRoot,
		OutputPath:  outputPath,
		Language:    lang,
		ExtraArgs:   extraArgs,
		NoUpload:    noUpload,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),

		Observe: &BuildObservation{CacheStatus: buildreport.CacheUnknown},
	}

	return &BuildContext{
		Config:  config,
		Lang:    lang,
		Builder: builder,
	}, nil
}

// ExecuteBuild runs the build process.
func (bm *BuildManager) ExecuteBuild(ctx *BuildContext) error {
	ctx.Config.BuildStartTime = time.Now()

	err := bm.runBuilders(ctx)

	ctx.Config.BuildEndTime = time.Now()

	if ctx.Config.Observe != nil && ctx.Config.Observe.CompilerVersion == "" {
		if v := DetectCompilerVersion(ctx.Lang); v != "" {
			ctx.Config.Observe.CompilerVersion = v
		}
	}

	return err
}

func (bm *BuildManager) runBuilders(ctx *BuildContext) error {
	if ctx.Lang == Rust {
		if err := BuildAndCopyRust(*ctx.Config); err != nil {
			return err
		}
		return nil
	}

	return ctx.Builder.Build(*ctx.Config)
}

// GetBuildDuration returns the duration of the build
func (bm *BuildManager) GetBuildDuration(ctx *BuildContext) time.Duration {
	if ctx.Config.BuildEndTime.IsZero() || ctx.Config.BuildStartTime.IsZero() {
		return 0
	}
	return ctx.Config.BuildEndTime.Sub(ctx.Config.BuildStartTime)
}

// IsToolInstalled checks if a required tool is installed
func (bm *BuildManager) IsToolInstalled(tool string) bool {
	_, err := exec.LookPath(tool)
	return err == nil
}

// ValidateTools checks if all required tools for a language are installed
func (bm *BuildManager) ValidateTools(lang Language) error {
	switch lang {
	case Go:
		if !bm.IsToolInstalled("go") {
			return phelixerr.New(
				phelixerr.CodeToolchainNotFound,
				"Go toolchain not found. Please install Go from https://golang.org/dl",
			)
		}
	case Rust:
		if !bm.IsToolInstalled("cargo") {
			return phelixerr.New(
				phelixerr.CodeToolchainNotFound,
				"Rust toolchain not found. Please install Rust from https://rustup.rs",
			)
		}
	default:
		return phelixerr.Newf(phelixerr.CodeUnsupportedProject, "unknown language: %s", lang)
	}
	return nil
}

// GetSupportedLanguages returns all supported languages
func (bm *BuildManager) GetSupportedLanguages() []Language {
	return bm.factory.GetSupportedLanguages()
}

// FormatLanguage returns a formatted string representation of a language
func (bm *BuildManager) FormatLanguage(lang Language) string {
	switch lang {
	case Go:
		return "Go (golang)"
	case Rust:
		return "Rust"
	default:
		return "Unknown"
	}
}
