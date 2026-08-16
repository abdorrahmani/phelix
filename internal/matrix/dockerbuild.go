package matrix

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
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
	logLine("command:  docker %s", redactCommand(args))

	cmd := exec.CommandContext(ctx, "docker", args...)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		result.Status = "failed"
		// Keep the *exec.ExitError reachable; low-level docker output goes only
		// to the debug log, never into the structured error. Build args already
		// redacted above, so the command line in the log is safe.
		result.Error = phelixerr.Wrapf(phelixerr.CodeDocker, err, "docker build failed for %s", c.Platform)
		logLine("error: %v", err)
		if s := stderr.String(); s != "" {
			logLine("stderr: %s", phelixerr.Redact(s))
		}
		return result
	}

	result.Duration = time.Since(start)
	result.Status = "success"
	result.Artifact = imageName
	return result
}

// redactCommand renders an exec command-line for a debug log, masking the
// VALUES of KEY=value-style arguments (e.g. "--build-arg DB_PASSWORD=hunter2").
// Keys are preserved so the log stays actionable; values never appear. This is
// defense in depth on top of [phelixerr.Redact]: build args can carry registry
// credentials, tokens and secrets, and the debug log is captured into the
// per-combination result before any downstream redaction sees it.
func redactCommand(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		// "--build-arg" and "--label" take their payload as the NEXT argument:
		// "--build-arg", "KEY=value". Mask the payload but keep the flag and key.
		if (a == "--build-arg" || a == "--label") && i+1 < len(args) {
			out[i] = a
			next := args[i+1]
			if k, v, ok := strings.Cut(next, "="); ok && k != "" && v != "" {
				out[i+1] = k + "=***"
			} else {
				out[i+1] = "***"
			}
			continue
		}
		// Joined form: "--build-arg=KEY=value", or any other KEY=value arg.
		if k, v, ok := strings.Cut(a, "="); ok && k != "" && v != "" && k != "--build-arg" && k != "--label" {
			out[i] = k + "=***"
			continue
		}
		out[i] = a
	}
	return strings.Join(out, " ")
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
		return phelixerr.Newf(
			phelixerr.CodeDocker,
			"refusing to push: %d of %d combinations failed to build. "+
				"Fix the failures and re-run, or use --push-partial to push only the %d successful images.\n"+
				"Failed combinations:\n%s",
			len(failed), len(results), len(succeeded),
			formatFailedList(failed))
	}

	// Fail-closed: no successful images at all — nothing to push.
	if len(succeeded) == 0 {
		if len(failed) > 0 {
			return phelixerr.Newf(phelixerr.CodeDocker, "nothing to push: all %d combinations failed", len(failed))
		}
		return phelixerr.New(phelixerr.CodeDocker, "nothing to push: no images to push")
	}

	for _, r := range succeeded {
		if r.Artifact == "" {
			continue
		}
		if err := d.pushOne(r.Artifact); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeDocker, err, "push %s", r.Artifact)
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
		return nil, phelixerr.New(phelixerr.CodeDocker, "no images to create manifest from")
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
		return nil, phelixerr.New(phelixerr.CodeDocker, "no successful images to create manifest from")
	}

	args := append([]string{"buildx", "imagetools", "create", "-t", manifestTag}, refs...)
	cmd := exec.CommandContext(ctx, "docker", args...)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Keep the *exec.ExitError reachable; underlying docker output is not
		// needed in the structured error.
		return nil, phelixerr.Wrapf(phelixerr.CodeDocker, err, "docker buildx imagetools create failed for %s", manifestTag)
	}

	return &Result{
		Combination: Combination{Lang: results[0].Combination.Lang},
		Status:      "success",
		Artifact:    manifestTag,
	}, nil
}

func (d *DockerMatrixBuilder) pushOne(imageRef string) error {
	cmd := exec.Command("docker", "push", imageRef)
	_, err := cmd.CombinedOutput()
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeDocker, err, "push failed for %s", imageRef)
	}
	return nil
}

func formatFailedList(results []Result) string {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "  - %s: %v\n", r.Combination.ID(), phelixerr.Redact(r.Error.Error()))
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

	_, err := cmd.CombinedOutput()
	if err != nil {
		// Never embed the raw docker output (it repeats build args); the
		// *exec.ExitError with exit code preserves the root cause.
		return nil, phelixerr.Wrapf(phelixerr.CodeDocker, err, "docker build failed for %s", imageName)
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
	_, err := cmd.CombinedOutput()
	if err != nil {
		// The full buildx version output is not useful in the error and could
		// contain environment info; keep the exit-status cause and a hint.
		return phelixerr.Wrapf(phelixerr.CodeDocker, err,
			"docker buildx is required for multi-arch builds but is not available; install BuildKit: https://docs.docker.com/build/buildx/install/")
	}
	return nil
}
