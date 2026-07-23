package docker

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/fatih/color"
)

// BuildConfig holds parameters for a Docker image build.
type BuildConfig struct {
	ProjectRoot string            // Directory containing the Dockerfile
	ImageName   string            // Full image name (registry/repo:tag)
	BuildArgs   map[string]string // Extra --build-arg KEY=value pairs
	Labels      map[string]string // OCI-standard labels applied during build
}

// BuildResult holds the outcome of a docker build.
type BuildResult struct {
	ImageID  string
	Duration time.Duration
	Output   string
}

// BuildImage builds a Docker image by shelling out to the `docker` CLI.
//
// SDK vs CLI tradeoff:
// We chose to shell out to the `docker` CLI rather than using the Go SDK
// (github.com/docker/docker/client) for these reasons:
//
//  1. Zero extra dependencies: the SDK pulls in the entire Docker engine API
//     client (~50+ transitive dependencies), increasing binary size and
//     compile time for a CLI tool.
//
//  2. Simpler auth model: `docker build` leverages the user's existing
//     `docker login` credentials automatically. With the SDK you must
//     manually resolve auth configs, handle credential helpers, and manage
//     TLS certificate verification — significant complexity for marginal gain.
//
//  3. BuildKit support: modern Docker uses BuildKit by default. The SDK
//     client's image build API predates BuildKit and doesn't fully support
//     its features. The CLI transparently uses BuildKit when available.
//
//  4. Error handling tradeoff: the SDK gives structured errors and streaming
//     build output via the engine API. Shelling out requires parsing stderr
//     for error messages. However, Docker CLI errors are already formatted
//     for human readability, so passing them through is sufficient for our
//     use case — a CLI tool that prints errors to the user.
//
//  5. Platform portability: the CLI works identically on Linux, macOS, and
//     Windows (via Docker Desktop), whereas the SDK requires handling
//     different socket paths per platform.
//
// Output is streamed in real-time with a dim prefix so the user sees Docker's
// progress (layer pulls, compilation steps) without the UI feeling stalled.
// Each line is prefixed with "    │ " to visually separate build output from
// Phelix's own step indicators.
func BuildImage(cfg BuildConfig) (*BuildResult, error) {
	start := time.Now()

	args := []string{"build", "-t", cfg.ImageName}

	// Add build arguments
	for k, v := range cfg.BuildArgs {
		args = append(args, "--build-arg", fmt.Sprintf("%s=%s", k, v))
	}

	// Add OCI labels via --label
	for k, v := range cfg.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", k, v))
	}

	// Add the build context directory
	args = append(args, cfg.ProjectRoot)

	cmd := exec.Command("docker", args...)
	cmd.Dir = cfg.ProjectRoot

	// Stream stdout and stderr in real-time instead of buffering.
	// Docker BuildKit writes progress to stderr and final status to stdout.
	// We merge both streams into a single indented view.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start docker build: %w", err)
	}

	// Stream both pipes concurrently; collect all output for error reporting.
	var output strings.Builder
	dim := color.New(color.Faint)

	// Stream stdout (build results, naming)
	go streamPipe(stdout, &output, dim)
	// Stream stderr (BuildKit progress, layer downloads)
	go streamPipe(stderr, &output, dim)

	// Wait for the build to finish
	err = cmd.Wait()

	result := &BuildResult{
		Duration: time.Since(start),
		Output:   output.String(),
	}

	if err != nil {
		return result, fmt.Errorf("docker build failed: %w", err)
	}

	// Extract the image ID from output if available
	result.ImageID = extractImageID(output.String())

	return result, nil
}

// streamPipe reads lines from r, prefixes each with a dim "    │ " indicator,
// writes them to the terminal, and appends to the output buffer.
func streamPipe(r io.Reader, output *strings.Builder, dim *color.Color) {
	scanner := bufio.NewScanner(r)
	// Increase buffer size for long lines (e.g. base64-encoded layers)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// Print with dim prefix so build output is visually distinct
		fmt.Printf("    %s %s\n", dim.Sprint("│"), line)
		output.WriteString(line + "\n")
	}
}

// TagImage tags a built image with a new tag.
func TagImage(sourceImage, targetImage string) error {
	cmd := exec.Command("docker", "tag", sourceImage, targetImage)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker tag failed: %w\nOutput:\n%s", err, string(output))
	}
	return nil
}

func extractImageID(output string) string {
	// Docker build output (BuildKit) contains lines like:
	// #3 => naming to docker.io/library/myimage:latest
	// We look for the image reference in the "naming to" line.
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.Contains(line, "naming to") {
			parts := strings.SplitN(line, "naming to ", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

// CheckDockerAvailable verifies that the docker CLI is installed and accessible.
func CheckDockerAvailable() error {
	cmd := exec.Command("docker", "version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker is not available or not running: %w\nOutput:\n%s", err, string(output))
	}
	return nil
}

// IsDockerRunning checks if the Docker daemon is reachable.
func IsDockerRunning() bool {
	return CheckDockerAvailable() == nil
}

// Cleanup removes dangling images produced by the build (best-effort).
func Cleanup(projectRoot string) {
	// Remove dangling images (those with <none>:<none> tag)
	cmd := exec.Command("docker", "image", "prune", "-f")
	_ = cmd.Run()
}
