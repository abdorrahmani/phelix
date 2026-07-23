package matrix

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DockerMatrixBuilder handles Docker image matrix builds.
//
// Two tagging strategies:
//
//  1. Per-combination tags (--matrix-tags):
//     Each {toolchain version} × {platform} gets its own image tag:
//     myapp:go1.22-linux-amd64
//     myapp:go1.23-linux-arm64
//     These are useful for debugging, testing, and pinning specific combinations.
//
//  2. Multi-arch manifest list (--multi-arch-tag):
//     A single tag that points to a manifest list containing all platform variants:
//     myapp:latest  →  manifest list [linux/amd64, linux/arm64]
//     Built via `docker buildx build --platform linux/amd64,linux/arm64`.
//     This is what production deployments typically use — Docker automatically
//     pulls the correct architecture at runtime.
//
// Push semantics (fail-closed by default):
//   - If ANY combination failed to build, we refuse to push any image.
//     Rationale: publishing a partial release (e.g. amd64 works but arm64 is
//     broken) is worse than publishing nothing. Users who want to push the
//     successful subset can use --push-partial.
//   - If ALL combinations succeeded, all images are pushed.
//   - --push-partial overrides the fail-closed default and pushes only the
//     successfully-built images.
type DockerMatrixBuilder struct {
	ProjectRoot string
	Registry    string // optional registry prefix (e.g. ghcr.io/user)
	Tag         string // version tag for the multi-arch manifest (e.g. "v1.2.3")
	BuildArgs   map[string]string
	Labels      map[string]string
	Debug       bool
}

// DockerBuildConfig controls what the matrix Docker builder produces.
type DockerBuildConfig struct {
	MatrixTags   bool // produce per-combination tags
	MultiArchTag bool // produce a multi-arch manifest list tag
	Push         bool // push images after building
	PushPartial  bool // push only successful images even if some failed
}

// BuildDockerImage builds a Docker image for one combination.
//
// For per-combination tags, we use plain `docker build -t <tag>` for each
// platform. The image is single-platform (the host platform unless buildx
// is used with --platform).
//
// For multi-arch manifest lists, we build once with `docker buildx build
// --platform <all-platforms>` which creates a manifest list. This is only
// done once (for the "latest" or version tag), not per-combination.
func (d *DockerMatrixBuilder) BuildDockerImage(ctx context.Context, c Combination) *Result {
	result := &Result{Combination: c}
	start := time.Now()

	logLine := func(format string, args ...any) {
		if d.Debug {
			result.Log += fmt.Sprintf(format, args...) + "\n"
		}
	}

	// Build the per-combination image tag.
	imageName := c.ImageTag("phelix-build") // temporary local tag
	if d.Registry != "" {
		imageName = d.Registry + "/" + imageName
	}

	args := []string{"build", "-t", imageName}

	// Add platform-specific build arg so the Dockerfile can detect the target.
	if d.BuildArgs == nil {
		d.BuildArgs = make(map[string]string)
	}
	d.BuildArgs["TARGETPLATFORM"] = c.Platform
	d.BuildArgs["TARGETOS"] = c.OS
	d.BuildArgs["TARGETARCH"] = c.Arch

	for k, v := range d.BuildArgs {
		args = append(args, "--build-arg", fmt.Sprintf("%s=%s", k, v))
	}
	for k, v := range d.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", k, v))
	}

	// For non-host platforms, use buildx with --platform.
	args = append(args, "--platform", c.Platform)
	args = append(args, d.ProjectRoot)

	logLine("image:    %s", imageName)
	logLine("platform: %s", c.Platform)
	logLine("command:  docker %s", strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, "docker", args...)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		result.Status = "failed"
		result.Error = fmt.Errorf("docker build failed for %s: %w\n%s", c.Platform, err, stderr.String())
		logLine("error: %v", err)
		logLine("stderr: %s", stderr.String())
		return result
	}

	result.Duration = time.Since(start)
	result.Status = "success"
	result.Artifact = imageName
	return result
}

// PushImages pushes all successfully-built images. This enforces fail-closed
// semantics: if any combination failed, the entire push is blocked unless
// pushPartial is true.
//
// Why fail-closed for push:
//   - A partial release (e.g. amd64 ok, arm64 broken) silently breaks
//     deployments on arm64 nodes. Users won't know until runtime.
//   - The cost of blocking is low (re-run after fixing the broken combo),
//     while the cost of a partial push is high (broken production).
//   - This mirrors how package managers handle atomic releases: either all
//     artifacts are published or none are.
func (d *DockerMatrixBuilder) PushImages(results []Result, pushPartial bool) error {
	succeeded := Succeeded(results)
	failed := Failed(results)

	if len(failed) > 0 && !pushPartial {
		return fmt.Errorf(
			"refusing to push: %d of %d combinations failed to build. "+
				"Fix the failures and re-run, or use --push-partial to push only the %d successful images.\n"+
				"Failed combinations:\n%s",
			len(failed), len(results), len(succeeded),
			formatFailedList(failed))
	}

	if len(succeeded) == 0 {
		return fmt.Errorf("nothing to push: all combinations failed")
	}

	for _, r := range succeeded {
		if r.Artifact == "" {
			continue
		}
		if err := d.pushOne(r.Artifact); err != nil {
			return fmt.Errorf("push %s: %w", r.Artifact, err)
		}
	}

	return nil
}

// BuildMultiArchManifest builds a single multi-arch manifest list using
// docker buildx. This creates a unified tag that Docker automatically
// resolves to the correct platform at pull time.
//
// The manifest list is built from all the per-combination images that
// were already built and tagged locally. We use `docker buildx imagetools
// create` to assemble the manifest without re-building.
func (d *DockerMatrixBuilder) BuildMultiArchManifest(ctx context.Context, results []Result, tag string) (*Result, error) {
	if len(results) == 0 {
		return nil, fmt.Errorf("no images to create manifest from")
	}

	manifestTag := fmt.Sprintf("%s:%s", "phelix-build", tag)
	if d.Registry != "" {
		manifestTag = d.Registry + "/" + manifestTag
	}

	// Collect all per-combination image references.
	refs := make([]string, 0, len(results))
	for _, r := range results {
		if r.Status == "success" && r.Artifact != "" {
			refs = append(refs, r.Artifact)
		}
	}

	if len(refs) == 0 {
		return nil, fmt.Errorf("no successful images to create manifest from")
	}

	args := append([]string{"buildx", "imagetools", "create", "-t", manifestTag}, refs...)
	cmd := exec.CommandContext(ctx, "docker", args...)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker buildx imagetools create failed: %w\n%s", err, stderr.String())
	}

	return &Result{
		Combination: Combination{Lang: results[0].Combination.Lang},
		Status:      "success",
		Artifact:    manifestTag,
	}, nil
}

func (d *DockerMatrixBuilder) pushOne(imageRef string) error {
	cmd := exec.Command("docker", "push", imageRef)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("push failed: %w\nOutput:\n%s", err, string(output))
	}
	return nil
}

func formatFailedList(results []Result) string {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "  - %s: %v\n", r.Combination.ID(), r.Error)
	}
	return b.String()
}

// BuildImage is a lower-level helper that builds a single Docker image
// without matrix context. Used by the non-matrix dockerize path.
func BuildImageSimple(ctx context.Context, projectRoot, imageName string, buildArgs map[string]string) (*Result, error) {
	start := time.Now()

	args := []string{"build", "-t", imageName}
	for k, v := range buildArgs {
		args = append(args, "--build-arg", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, projectRoot)

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = projectRoot

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker build failed: %w\n%s", err, string(output))
	}

	return &Result{
		Status:   "success",
		Artifact: imageName,
		Duration: time.Since(start),
	}, nil
}

// Ensure Docker buildx is available for multi-platform builds.
func EnsureBuildx() error {
	cmd := exec.Command("docker", "buildx", "version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"docker buildx is required for multi-arch builds but is not available: %w\n%s"+
				"Install BuildKit: https://docs.docker.com/build/buildx/install/",
			err, string(output))
	}
	return nil
}
