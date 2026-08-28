package builder

import (
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/buildreport"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Language represents a programming language supported by Phelix
type Language string

const (
	Go   Language = "go"
	Rust Language = "rust"
)

// ParseLanguage converts a string to a Language type
func ParseLanguage(s string) Language {
	lang := Language(strings.ToLower(strings.TrimSpace(s)))
	switch lang {
	case Go, Rust:
		return lang
	default:
		return Language("unknown")
	}
}

// String returns the string representation of the language
func (l Language) String() string {
	return string(l)
}

// IsSupported returns true if the language is supported
func (l Language) IsSupported() bool {
	return l == Go || l == Rust
}

// BuildConfig contains configuration for the build process
type BuildConfig struct {
	ID             string    // Application ID
	Name           string    // Application name
	ProjectRoot    string    // Root directory of the project
	OutputPath     string    // Path where built binary should be placed
	Language       Language  // Programming language
	ExtraArgs      []string  // Extra build arguments
	NoUpload       bool      // Whether to upload to server after building
	CreatedAt      time.Time // When app was created
	UpdatedAt      time.Time // Last update time
	BuildStartTime time.Time // When build started (set during build)
	BuildEndTime   time.Time // When build ended (set after build)

	// Observe, when non-nil, receives facts observed during the build
	// (compiler-cache status, detected toolchain version). PrepareBuild wires
	// it automatically; direct BuilderInterface users may leave it nil —
	// builders must tolerate a nil pointer (additive, optional).
	Observe *BuildObservation
}

// BuildObservation collects non-fatal telemetry emitted by one build run.
// Builders populate it best-effort: an undeterminable value simply stays
// unknown and never influences build success.
type BuildObservation struct {
	// CacheStatus records whether the compile came from cache
	// (buildreport.CacheHit), did real compilation work (buildreport.CacheCold)
	// or could not be determined (buildreport.CacheUnknown).
	CacheStatus buildreport.CacheStatus
	// CacheSource names the cache mechanism when known.
	CacheSource string
	// CompilerVersion holds the normalized toolchain version (major.minor,
	// e.g. "1.27"), probed by the build manager after execution.
	CompilerVersion string
}

// BuildResult contains the result of a build operation
type BuildResult struct {
	Success      bool
	BinaryPath   string
	Output       string
	Error        error
	Duration     time.Duration
	LanguageUsed Language
}

// BuilderInterface defines the contract for building projects in different languages
type BuilderInterface interface {
	// Name returns the builder's language name
	Name() Language

	// Validate checks if the project is valid for this builder
	Validate(projectRoot string) error

	// Build performs the build operations
	Build(config BuildConfig) error

	// GetBinaryPath returns the path to the built binary after a successful build
	GetBinaryPath(config BuildConfig) (string, error)

	// GetBuildFlags returns supported build flags/options for this language
	GetBuildFlags() []string
}

// BuilderFactory creates a builder for the given language
type BuilderFactory struct {
	builders map[Language]BuilderInterface
}

// NewBuilderFactory creates a new builder factory
func NewBuilderFactory() *BuilderFactory {
	bf := &BuilderFactory{
		builders: make(map[Language]BuilderInterface),
	}
	// Register builders
	bf.Register(Go, NewGoBuilder())
	bf.Register(Rust, NewRustBuilder())
	return bf
}

// Register registers a builder for a language
func (bf *BuilderFactory) Register(lang Language, builder BuilderInterface) {
	bf.builders[lang] = builder
}

// GetBuilder returns a builder for the given language
func (bf *BuilderFactory) GetBuilder(lang Language) (BuilderInterface, error) {
	if builder, exists := bf.builders[lang]; exists {
		return builder, nil
	}
	return nil, phelixerr.Newf(phelixerr.CodeUnsupportedProject, "unsupported language: %s", lang)
}

// GetSupportedLanguages returns a list of supported languages
func (bf *BuilderFactory) GetSupportedLanguages() []Language {
	langs := make([]Language, 0, len(bf.builders))
	for lang := range bf.builders {
		langs = append(langs, lang)
	}
	return langs
}
