package docker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DockerfileGenerator produces multi-stage Dockerfiles optimized for layer caching.
type DockerfileGenerator struct {
	ProjectRoot string
	Language    Language
}

// NewDockerfileGenerator creates a generator for the given project.
func NewDockerfileGenerator(projectRoot string, lang Language) *DockerfileGenerator {
	return &DockerfileGenerator{ProjectRoot: projectRoot, Language: lang}
}

// HasExistingDockerfile reports whether a user-provided Dockerfile already exists.
func (g *DockerfileGenerator) HasExistingDockerfile() bool {
	_, err := os.Stat(filepath.Join(g.ProjectRoot, "Dockerfile"))
	return err == nil
}

// Generate produces a multi-stage Dockerfile string optimized for Docker layer caching.
//
// Caching layer ordering rationale:
//
// For Go:
//  1. Builder stage: Copy go.mod + go.sum FIRST, then run `go mod download`.
//     This creates a cached layer for all dependency downloads. Docker rebuilds
//     this layer only when go.mod or go.sum change, not on every source edit.
//  2. Copy the rest of the source AFTER dependencies are cached.
//  3. Final stage: minimal alpine image with just the static binary.
//
// For Rust:
//  1. Builder stage: Copy Cargo.toml + Cargo.lock, create a dummy src/main.rs
//     that just contains `fn main() {}`, and run `cargo build --release`.
//     This caches ALL dependency compilation (~minutes for typical Rust projects)
//     separately from application code compilation.
//  2. Remove the dummy source, copy the real source, and rebuild.
//     Only the application's own code needs recompilation.
//  3. Final stage: debian:slim for glibc compatibility (or alpine for musl).
//
// This ordering means: edit source → only recompile app code (seconds),
// not dependencies (minutes). Edit Cargo.toml/go.mod → recompile deps.
func (g *DockerfileGenerator) Generate() string {
	switch g.Language {
	case LanguageGo:
		return g.generateGo()
	case LanguageRust:
		return g.generateRust()
	default:
		return ""
	}
}

func (g *DockerfileGenerator) generateGo() string {
	// --- Stage 1: Builder ---
	// Uses official Go image. Dependencies (go.mod/go.sum) are copied and
	// `go mod download` runs before copying any source code. This means the
	// dependency download layer is cached as long as go.mod/go.sum don't change.
	var b strings.Builder
	b.WriteString("# Stage 1: Build\n")
	b.WriteString("FROM golang:1.23-alpine AS builder\n\n")
	b.WriteString("WORKDIR /app\n\n")
	// Dependency layer — cached unless go.mod/go.sum change
	b.WriteString("# Copy dependency manifests first for layer caching\n")
	b.WriteString("COPY go.mod go.sum ./\n")
	b.WriteString("RUN go mod download\n\n")
	// Source layer — only rebuilt when source changes
	b.WriteString("# Copy source code\n")
	b.WriteString("COPY . .\n\n")
	// Build with CGO_ENABLED=0 for a fully static binary compatible with scratch.
	b.WriteString("RUN CGO_ENABLED=0 GOOS=linux go build -ldflags=\"-s -w\" -o /app/server .\n\n")

	// --- Stage 2: Runtime ---
	// scratch for a statically-linked binary (smallest possible image).
	// If the binary needs libc (e.g. uses netgo with cgo), switch to alpine.
	b.WriteString("# Stage 2: Runtime (minimal image)\n")
	b.WriteString("FROM scratch\n\n")
	b.WriteString("COPY --from=builder /app/server /server\n\n")
	b.WriteString("ENTRYPOINT [\"/server\"]\n")
	return b.String()
}

func (g *DockerfileGenerator) generateRust() string {
	var b strings.Builder

	// --- Stage 1: Dependency cache ---
	// The trick: copy only Cargo.toml + Cargo.lock, create a dummy main.rs,
	// and build. Cargo resolves and compiles ALL dependencies, which Docker
	// caches. Next build: only the real source changes trigger recompilation
	// of the application code, not the entire dependency tree.
	b.WriteString("# Stage 1: Dependency cache\n")
	b.WriteString("FROM rust:1.80-slim AS deps\n\n")
	b.WriteString("WORKDIR /app\n\n")
	// Copy dependency manifests
	b.WriteString("# Copy only Cargo files for dependency caching\n")
	b.WriteString("COPY Cargo.toml Cargo.lock ./\n\n")
	// Create dummy main.rs to satisfy Cargo's requirement for src/main.rs
	b.WriteString("# Create dummy main.rs to cache dependency compilation\n")
	b.WriteString("RUN mkdir src && echo 'fn main() { println!(\"placeholder\"); }' > src/main.rs\n")
	b.WriteString("RUN cargo build --release 2>/dev/null || true\n")
	b.WriteString("RUN rm -rf src\n\n")

	// --- Stage 2: Real build ---
	b.WriteString("# Stage 2: Build with real source\n")
	b.WriteString("FROM rust:1.80-slim AS builder\n\n")
	b.WriteString("WORKDIR /app\n\n")
	b.WriteString("# Copy dependency manifests and pre-compiled deps\n")
	b.WriteString("COPY Cargo.toml Cargo.lock ./\n")
	b.WriteString("COPY --from=deps /app/target /app/target\n\n")
	b.WriteString("# Copy real source code\n")
	b.WriteString("COPY src ./src\n\n")
	b.WriteString("# Rebuild only application code (deps are cached)\n")
	b.WriteString("RUN cargo build --release\n\n")
	// Find the binary name from Cargo.toml (convention: package name)
	b.WriteString("# Locate the compiled binary\n")
	b.WriteString("RUN cp target/release/$(basename $(dirname $(find target/release -maxdepth 1 -type f -perm /111 ! -name '*.d' | head -1))) /app/server 2>/dev/null || \\\n")
	b.WriteString("    cp target/release/* /app/server 2>/dev/null || true\n\n")

	// --- Stage 3: Runtime ---
	b.WriteString("# Stage 3: Runtime (minimal image)\n")
	b.WriteString("FROM debian:bookworm-slim AS runtime\n\n")
	b.WriteString("RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*\n\n")
	b.WriteString("COPY --from=builder /app/server /server\n\n")
	b.WriteString("ENTRYPOINT [\"/server\"]\n")
	return b.String()
}

// WriteDockerfile writes the generated Dockerfile to projectRoot/Dockerfile.
func (g *DockerfileGenerator) WriteDockerfile() error {
	content := g.Generate()
	if content == "" {
		return fmt.Errorf("unsupported language: %s", g.Language)
	}
	return os.WriteFile(filepath.Join(g.ProjectRoot, "Dockerfile"), []byte(content), 0o644)
}
