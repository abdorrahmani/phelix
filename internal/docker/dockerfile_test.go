package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerfileGenerator_HasExistingDockerfile(t *testing.T) {
	dir := t.TempDir()

	gen := NewDockerfileGenerator(dir, LanguageGo)
	if gen.HasExistingDockerfile() {
		t.Error("should not find Dockerfile in empty directory")
	}

	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !gen.HasExistingDockerfile() {
		t.Error("should find Dockerfile after creating one")
	}
}

// TestGoDockerfile_LayerOrdering verifies that dependency files are copied
// before source code in the generated Dockerfile. This is critical for
// Docker layer caching: if go.mod/go.sum are copied and `go mod download`
// runs before the rest of the source is copied, Docker caches the
// dependency download layer and only rebuilds it when dependencies change.
func TestGoDockerfile_LayerOrdering(t *testing.T) {
	gen := NewDockerfileGenerator("/tmp/test", LanguageGo)
	dockerfile := gen.Generate()

	// Find the positions of key instructions
	goModPos := strings.Index(dockerfile, "COPY go.mod go.sum")
	downloadPos := strings.Index(dockerfile, "go mod download")
	sourceCopyPos := strings.Index(dockerfile, "COPY . .")
	buildPos := strings.Index(dockerfile, "go build")

	if goModPos == -1 {
		t.Fatal("missing 'COPY go.mod go.sum' instruction")
	}
	if downloadPos == -1 {
		t.Fatal("missing 'go mod download' instruction")
	}
	if sourceCopyPos == -1 {
		t.Fatal("missing 'COPY . .' instruction")
	}
	if buildPos == -1 {
		t.Fatal("missing 'go build' instruction")
	}

	// Dependencies must be copied before source code
	if goModPos > sourceCopyPos {
		t.Error("go.mod/go.sum must be COPY'd before source code for layer caching")
	}
	// go mod download must run after copying dependency manifests
	if downloadPos < goModPos {
		t.Error("go mod download must run after COPY go.mod go.sum")
	}
	// go mod download must run before copying source
	if downloadPos > sourceCopyPos {
		t.Error("go mod download must run before COPY . .")
	}
	// Build must happen after source copy
	if buildPos < sourceCopyPos {
		t.Error("go build must run after COPY . .")
	}
}

// TestRustDockerfile_LayerOrdering verifies the Rust dependency caching trick:
// Cargo.toml/Cargo.lock are copied and a dummy main.rs is compiled first to
// cache all dependency compilation. The real source is copied after, so only
// application code needs recompilation on source changes.
func TestRustDockerfile_LayerOrdering(t *testing.T) {
	gen := NewDockerfileGenerator("/tmp/test", LanguageRust)
	dockerfile := gen.Generate()

	cargoCopyPos := strings.Index(dockerfile, "COPY Cargo.toml Cargo.lock")
	dummyMainPos := strings.Index(dockerfile, "fn main()")
	cargoBuildPos := strings.Index(dockerfile, "cargo build --release")
	realSourcePos := strings.Index(dockerfile, "COPY src ./src")
	realBuildPos := strings.LastIndex(dockerfile, "cargo build --release")

	if cargoCopyPos == -1 {
		t.Fatal("missing 'COPY Cargo.toml Cargo.lock' instruction")
	}
	if dummyMainPos == -1 {
		t.Fatal("missing dummy main.rs creation for dependency caching")
	}
	if cargoBuildPos == -1 {
		t.Fatal("missing initial 'cargo build --release' for dependency caching")
	}
	if realSourcePos == -1 {
		t.Fatal("missing 'COPY src ./src' for real source")
	}

	// Cargo files must be copied first
	if dummyMainPos < cargoCopyPos {
		t.Error("dummy main.rs must be created after copying Cargo files")
	}
	// Dependency build must happen before real source copy
	if cargoBuildPos > realSourcePos {
		t.Error("dependency compilation must happen before copying real source")
	}
	// Real source must be copied before final build
	if realBuildPos < realSourcePos {
		t.Error("final cargo build must happen after copying real source")
	}
}

func TestGoDockerfile_ToolchainVersionArg(t *testing.T) {
	dockerfile := NewDockerfileGenerator("/tmp/test", LanguageGo).Generate()
	if !strings.Contains(dockerfile, "ARG GO_VERSION=1.23") {
		t.Fatal("Go Dockerfile should declare GO_VERSION with the existing default")
	}
	if !strings.Contains(dockerfile, "FROM golang:${GO_VERSION}-alpine AS builder") {
		t.Fatal("Go builder image should consume GO_VERSION")
	}
}

func TestRustDockerfile_ToolchainVersionArg(t *testing.T) {
	dockerfile := NewDockerfileGenerator("/tmp/test", LanguageRust).Generate()
	if !strings.Contains(dockerfile, "ARG RUST_VERSION=1.80") {
		t.Fatal("Rust Dockerfile should declare RUST_VERSION with the existing default")
	}
	if strings.Count(dockerfile, "FROM rust:${RUST_VERSION}-slim") != 2 {
		t.Fatal("both Rust build stages should consume RUST_VERSION")
	}
}

func TestDockerfileGenerator_UnsupportedLanguage(t *testing.T) {
	gen := NewDockerfileGenerator("/tmp/test", Language("python"))
	if gen.Generate() != "" {
		t.Error("unsupported language should return empty string")
	}
}

func TestGoDockerfile_StaticBinary(t *testing.T) {
	gen := NewDockerfileGenerator("/tmp/test", LanguageGo)
	dockerfile := gen.Generate()

	// Should use CGO_ENABLED=0 for static binary
	if !strings.Contains(dockerfile, "CGO_ENABLED=0") {
		t.Error("Go Dockerfile should use CGO_ENABLED=0 for static binary")
	}
	// Should use scratch or alpine for minimal runtime
	if !strings.Contains(dockerfile, "FROM scratch") {
		t.Error("Go Dockerfile should use scratch for minimal runtime image")
	}
}

func TestRustDockerfile_MinimalRuntime(t *testing.T) {
	gen := NewDockerfileGenerator("/tmp/test", LanguageRust)
	dockerfile := gen.Generate()

	// Should use debian:slim for glibc compatibility
	if !strings.Contains(dockerfile, "debian:bookworm-slim") {
		t.Error("Rust Dockerfile should use debian:slim for runtime")
	}
}

// The manifest COPY instructions must use optional wildcards: a dependency-free
// Go project has no go.sum (and a fresh `cargo new` project no Cargo.lock), and
// an unconditional COPY of a missing file fails the whole build.
func TestDockerfile_OptionalManifestWildcards(t *testing.T) {
	goGen := NewDockerfileGenerator("/tmp/test", LanguageGo)
	if !strings.Contains(goGen.Generate(), "COPY go.mod go.sum* ./") {
		t.Fatal("Go Dockerfile must copy go.sum with the optional wildcard")
	}
	rustGen := NewDockerfileGenerator("/tmp/test", LanguageRust)
	rust := rustGen.Generate()
	if got := strings.Count(rust, "COPY Cargo.toml Cargo.lock* ./"); got != 2 {
		t.Fatalf("Rust Dockerfile must copy Cargo.lock with the optional wildcard in both stages, got %d", got)
	}
}
