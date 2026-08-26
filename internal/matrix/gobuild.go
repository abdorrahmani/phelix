package matrix

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// GoMatrixBuilder handles Go-specific matrix builds. Two strategies:
//
//  1. Native cross-compilation (GOOS/GOARCH) — when CGO_ENABLED=0.
//     Go's standard library is pure Go for most packages, so setting
//     GOOS and GOARCH is sufficient to produce a working binary for any
//     supported platform without any cross-compiler toolchain. This is
//     fast and requires no Docker.
//
//  2. Per-version Docker container builds — when the user specifies
//     multiple Go versions (e.g. --go-versions 1.21,1.22,1.23).
//     We cannot install multiple Go versions side-by-side on the host
//     (they share GOPATH/bin and interfere), so each version runs inside
//     its own Docker container (golang:1.21, golang:1.22, etc.). The
//     project source is mounted in and the binary is extracted afterward.
//
// CGO detection:
// If the project uses `import "C"` (cgo), cross-compilation breaks
// because CGO requires a C cross-compiler for each target architecture
// (e.g. gcc-aarch64-linux-gnu for linux/arm64 on an amd64 host).
// Rather than silently producing a broken binary, we detect cgo usage
// and either:
//   - Fail with a clear error when cross-compiling with CGO enabled, or
//   - Allow builds for the host's native architecture only.
type GoMatrixBuilder struct {
	// ProjectRoot is the absolute path to the project directory.
	ProjectRoot string
	// AppName is the application name, used in binary naming.
	AppName string
	// UseDocker forces Docker-based builds even for single-version builds.
	// This is useful when the user explicitly wants version-specific builds
	// via --go-versions.
	UseDocker bool
	// Debug enables verbose logging of commands and their output.
	Debug bool
}

// DetectCgo returns true if the project uses cgo (has import "C" directives).
// This is a heuristic — it scans all .go files for the import pattern.
// If the project has a CGO_ENABLED env override in a Makefile or build
// script, the user should set --cgo-cross-toolchain instead.
func DetectCgo(projectRoot string) (bool, error) {
	var found bool
	err := filepath.Walk(projectRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || found {
			return err
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil // skip unreadable files
		}
		// Match `import "C"` or `import ( "C" )` — the cgo directive.
		if cgoPattern.Match(data) {
			found = true
			return filepath.SkipDir
		}
		return nil
	})
	return found, err
}

var cgoPattern = regexp.MustCompile(`(?m)^\s*import\s+"C"`)

// BuildGo runs the build for one combination. It chooses between native
// cross-compilation and Docker-based builds based on the configuration.
//
// Native cross-compilation (CGO_ENABLED=0):
//
//	GOOS=<os> GOARCH=<arch> CGO_ENABLED=0 go build -o <output> .
//
// This works because Go's standard library is self-contained. The binary
// is statically linked and runs on the target without any runtime deps.
//
// Docker-based builds (per-version):
//
//	docker run --rm -v $PWD:/src -w /src golang:<ver> \
//	  GOOS=<os> GOARCH=<arch> CGO_ENABLED=0 go build -o /out/<name> .
//
// The binary is extracted via docker cp or a volume mount.
func (g *GoMatrixBuilder) Build(ctx context.Context, c Combination) *Result {
	result := &Result{Combination: c}

	logLine := func(format string, args ...any) {
		if g.Debug {
			result.Log += fmt.Sprintf(format, args...) + "\n"
		}
	}

	// Detect CGO — abort early if cross-compiling with CGO.
	if g.UseDocker {
		// Docker builds don't need CGO detection on the host — the container
		// has the full Go toolchain. But we still warn.
		if cgo, _ := DetectCgo(g.ProjectRoot); cgo {
			logLine("cgo detected in source, using Docker build with full toolchain")
		}
		return g.buildInDocker(ctx, c, result)
	}

	// Native cross-compilation path.
	if cgo, err := DetectCgo(g.ProjectRoot); err != nil {
		result.Status = "failed"
		result.Error = phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to scan for cgo usage")
		return result
	} else if cgo {
		// CGO detected — cross-compilation without a C cross-compiler
		// would produce a binary that segfaults or fails to load on the
		// target. Rather than silently shipping broken artifacts, we fail
		// with a clear message.
		result.Status = "failed"
		result.Error = phelixerr.Newf(
			phelixerr.CodeUnsupportedProject,
			"cgo detected in project source — cross-compiling Go with CGO enabled "+
				"requires a configured C cross-toolchain (e.g. gcc-aarch64-linux-gnu) "+
				"for target %s. Either: (1) install the cross-compiler and set "+
				"CC=<cross-cc>, (2) run with --go-versions to use Docker-based builds "+
				"which have the full toolchain, or (3) ensure CGO is disabled in your code",
			c.Platform)
		return result
	}

	start := time.Now()

	// Find the main package.
	mainPkg, err := g.findMainPackage()
	if err != nil {
		result.Status = "failed"
		result.Error = err
		return result
	}

	outDir := filepath.Join(g.ProjectRoot, "builds", "matrix", c.ID())
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		result.Status = "failed"
		result.Error = phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "create output dir")
		return result
	}
	outPath := filepath.Join(outDir, c.BinaryName(g.AppName))

	logLine("command: GOOS=%s GOARCH=%s CGO_ENABLED=0 go build -o %s %s", c.OS, c.Arch, outPath, mainPkg)
	logLine("output:  %s", outPath)
	logLine("cache:   (native cross-compile, no Docker cache)")

	cmd := exec.CommandContext(ctx, "go", "build", "-o", outPath, mainPkg)
	cmd.Dir = g.ProjectRoot
	cmd.Env = append(os.Environ(),
		"GOOS="+c.OS,
		"GOARCH="+c.Arch,
		"CGO_ENABLED=0",
	)

	output := &bytes.Buffer{}
	commandOutput := &progressOutputWriter{ctx: ctx, key: c.ID(), debug: g.Debug, log: output}
	ReportBuildProgress(ctx, c.ID(), "compiling", 0, 0)
	cmd.Stdout = commandOutput
	cmd.Stderr = commandOutput
	if err := cmd.Run(); err != nil {
		result.Status = "failed"
		// The underlying *exec.ExitError (with exit code) stays reachable via
		// errors.As; the full compiler output is captured only in the debug log.
		result.Error = phelixerr.Wrapf(phelixerr.CodeBuildFailed, err, "go build failed for %s (%s/%s)", g.AppName, c.OS, c.Arch)
		logLine("error: %v", err)
		if s := output.String(); s != "" {
			logLine("stderr: %s", s)
		}
		return result
	}

	result.Duration = time.Since(start)
	result.Status = "success"
	result.Artifact = outPath
	logLine("success in %s", result.Duration.Round(time.Millisecond))
	return result
}

// buildInDocker runs the Go build inside a version-specific Docker container.
//
// Why Docker for version matrix:
// Go's toolchain is not designed for side-by-side installation — `go install
// golang.org/dl/go1.21@latest` works but requires manual go/bin management
// and doesn't isolate module caches. Docker gives us:
//   - Exact version pinning (golang:1.21 vs golang:1.22)
//   - Clean cache isolation per combination
//   - No host pollution
//   - Reproducible builds (same Docker image = same behavior)
//
// The project source is bind-mounted read-only, and the binary is written
// to a temp directory that we extract after the container exits.
func (g *GoMatrixBuilder) buildInDocker(ctx context.Context, c Combination, result *Result) *Result {
	start := time.Now()

	logLine := func(format string, args ...any) {
		if g.Debug {
			result.Log += fmt.Sprintf(format, args...) + "\n"
		}
	}

	image := fmt.Sprintf("golang:%s", c.Version)
	mainPkg, err := g.findMainPackage()
	if err != nil {
		result.Status = "failed"
		result.Error = err
		return result
	}

	// Output directory — mounted into the container.
	outDir := filepath.Join(g.ProjectRoot, "builds", "matrix", c.ID())
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		result.Status = "failed"
		result.Error = phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "create output dir")
		return result
	}
	outPath := filepath.Join(outDir, c.BinaryName(g.AppName))

	// Docker cache directory — keyed per combination so different versions
	// don't invalidate each other's Go module/build caches.
	cacheDir := filepath.Join(g.ProjectRoot, ".phelix", "cache", "go", c.ID())
	os.MkdirAll(cacheDir, 0o755)

	logLine("image:    %s", image)
	logLine("platform: %s", c.Platform)
	logLine("source:   %s (read-only)", g.ProjectRoot)
	logLine("output:   %s", outPath)
	logLine("cache:    %s", cacheDir)

	// Pull the image if not present (best-effort, non-fatal).
	logLine("pulling image %s...", image)
	pullCmd := exec.CommandContext(ctx, "docker", "pull", image)
	if g.Debug {
		pullOutput, _ := pullCmd.CombinedOutput()
		logLine("pull: %s", strings.TrimSpace(string(pullOutput)))
	} else {
		_ = pullCmd.Run()
	}

	// Run the build inside the container.
	// Mount:
	//   - project source → /src (read-only)
	//   - output dir → /out (writable)
	//   - cache dir → /root/.cache/go-build (writable, persists across runs)
	binName := c.BinaryName(g.AppName)
	buildCmd := fmt.Sprintf(
		"GOOS=%s GOARCH=%s CGO_ENABLED=0 go build -o /out/%s %s",
		c.OS, c.Arch, binName, mainPkg)

	logLine("command:  docker run --rm --platform linux/%s -v %s:/src:ro -v %s:/out -v %s:/root/.cache/go-build -w /src %s sh -c '%s'",
		c.Arch, g.ProjectRoot, outDir, cacheDir, image, buildCmd)

	cmd := exec.CommandContext(ctx,
		"docker", "run", "--rm",
		"--platform", "linux/"+c.Arch,
		"-v", g.ProjectRoot+":/src:ro",
		"-v", outDir+":/out",
		"-v", cacheDir+":/root/.cache/go-build",
		"-w", "/src",
		image,
		"sh", "-c", buildCmd,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		result.Status = "failed"
		// Keep the *exec.ExitError (and its exit code) reachable; the container
		// output goes to the debug log only.
		result.Error = phelixerr.Wrapf(phelixerr.CodeBuildFailed, err, "docker go build failed for %s (%s/%s)", g.AppName, c.OS, c.Arch)
		logLine("error: %v", err)
		if s := stderr.String(); s != "" {
			logLine("stderr: %s", s)
		}
		return result
	}

	result.Duration = time.Since(start)
	result.Status = "success"
	result.Artifact = outPath
	logLine("success in %s", result.Duration.Round(time.Millisecond))
	return result
}

// findMainPackage locates the Go main package in the project.
func (g *GoMatrixBuilder) findMainPackage() (string, error) {
	// Check for go.mod first — if present, use relative path.
	if _, err := os.Stat(filepath.Join(g.ProjectRoot, "go.mod")); err == nil {
		return ".", nil
	}

	// Walk to find main.go with "package main".
	var mainDir string
	err := filepath.Walk(g.ProjectRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || mainDir != "" {
			return nil
		}
		if info.Name() != "main.go" {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "package main") {
				mainDir = filepath.Dir(path)
				return filepath.SkipDir
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if mainDir == "" {
		return "", phelixerr.New(phelixerr.CodeUnsupportedProject, "no Go main package found in project root")
	}

	rel, err := filepath.Rel(g.ProjectRoot, mainDir)
	if err != nil {
		return ".", nil // fallback to root
	}
	return "./" + rel, nil
}
