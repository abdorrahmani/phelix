package builder

import (
	"os/exec"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/buildreport"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

const DefaultBuildTimeout = 30 * time.Minute

// BuildManager orchestrates the build process
type BuildManager struct {
	factory *BuilderFactory

	// scanMu guards the scan cache below. Workspace builds share one manager
	// across service goroutines, and the cache is keyed by project root, so
	// concurrent scans of different services would otherwise race on it.
	scanMu  sync.Mutex
	scan    *ProjectScan
	scanFor string

	timeout time.Duration
}

// BuildContext wraps the BuildConfig and adds additional context
type BuildContext struct {
	Config  *BuildConfig
	Lang    Language
	Builder BuilderInterface
}

// NewBuildManager creates a new build manager
func NewBuildManager() *BuildManager {
	return &BuildManager{
		factory: NewBuilderFactory(),
	}
}

// scanProject scans the project directory and caches the result
func (bm *BuildManager) scanProject(root string) *ProjectScan {
	bm.scanMu.Lock()
	defer bm.scanMu.Unlock()

	if bm.scan != nil && bm.scanFor == root {
		return bm.scan
	}
	bm.scan = NewProjectScan(root)
	bm.scanFor = root
	return bm.scan
}

// DetectLanguage detects the programming language of a project
func (bm *BuildManager) DetectLanguage(projectRoot string) Language {
	return detectLanguageFrom(bm.scanProject(projectRoot))
}

// detectLanguageFrom determines the language based on the project scan.
func detectLanguageFrom(s *ProjectScan) Language {
	rustRoot := s.RootFile("Cargo.toml")
	goRoot := s.RootFile("main.go") || s.RootFile("go.mod")

	if rustRoot && goRoot {
		return Ambiguous
	}

	if rustRoot {
		return Rust
	}

	if goRoot || s.HasMainPackage() {
		return Go
	}

	return "unknown"
}

// PrepareBuild validates and prepares the build context
func (bm *BuildManager) PrepareBuild(
	appID string,
	appName string,
	projectRoot string,
	outputPath string,
	detectLang Language,
	extraArgs []string,
) (*BuildContext, error) {
	scan := bm.scanProject(projectRoot)

	lang := detectLang
	if !lang.IsSupported() {
		lang = detectLanguageFrom(scan)
	}

	if !lang.IsSupported() {
		return nil, phelixerr.Newf(phelixerr.CodeUnsupportedProject, "unsupported or unknown project language: %s", lang)
	}

	builder, err := bm.factory.GetBuilder(lang)
	if err != nil {
		return nil, err
	}

	if err := builder.Validate(scan); err != nil {
		return nil, err
	}

	config := &BuildConfig{
		ID:          appID,
		Name:        appName,
		ProjectRoot: projectRoot,
		OutputPath:  outputPath,
		Language:    lang,
		ExtraArgs:   extraArgs,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Scan:        scan,
		Timeout:     bm.timeout,

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

// SetTimeout bounds each compiler invocation. Zero keeps DefaultBuildTimeout.
func (bm *BuildManager) SetTimeout(d time.Duration) {
	bm.timeout = d
}

// buildTimeout returns the effective deadline for one invocation.
func buildTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultBuildTimeout
	}

	return d
}
