package builder

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
	// Check for Rust Cargo.toml first
	if _, err := os.Stat(filepath.Join(projectRoot, "Cargo.toml")); err == nil {
		return Rust
	}

	// Check for Go main
	if _, err := os.Stat(filepath.Join(projectRoot, "main.go")); err == nil {
		return Go
	}

	// Check for go.mod
	if _, err := os.Stat(filepath.Join(projectRoot, "go.mod")); err == nil {
		return Go
	}

	// Fallback: try to find any .go file with package main
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

		// Simple check for "package main"
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

// BuildConfig wraps the BuildConfig and adds additional context
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
	// Auto-detect language if not specified
	lang := detectLang
	if !lang.IsSupported() {
		lang = bm.DetectLanguage(projectRoot)
	}

	if !lang.IsSupported() {
		return nil, fmt.Errorf("unsupported or unknown project language: %s", lang)
	}

	// Get the builder for this language
	builder, err := bm.factory.GetBuilder(lang)
	if err != nil {
		return nil, err
	}

	// Validate the project
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
	}

	return &BuildContext{
		Config:  config,
		Lang:    lang,
		Builder: builder,
	}, nil
}

// ExecuteBuild runs the build process
func (bm *BuildManager) ExecuteBuild(ctx *BuildContext) error {
	ctx.Config.BuildStartTime = time.Now()

	// For Rust, we need special handling to copy the binary
	if ctx.Lang == Rust {
		if err := BuildAndCopyRust(*ctx.Config); err != nil {
			return err
		}
	} else {
		// For other languages (Go, etc.), use the standard builder
		if err := ctx.Builder.Build(*ctx.Config); err != nil {
			return err
		}
	}

	ctx.Config.BuildEndTime = time.Now()
	return nil
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
			return fmt.Errorf("Go toolchain not found. Please install Go from https://golang.org/dl")
		}
	case Rust:
		if !bm.IsToolInstalled("cargo") {
			return fmt.Errorf("Rust toolchain not found. Please install Rust from https://rustup.rs")
		}
	default:
		return fmt.Errorf("unknown language: %s", lang)
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
