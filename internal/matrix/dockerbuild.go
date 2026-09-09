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
// Tagging: each {toolchain version} × {platform} combination gets its own
// image tag, derived from the app name and the user-supplied tag:
//
//	myapp:v1.2.3-go1.22-amd64       (tag given)
//	myapp:latest-go1.22-amd64       (no tag)
//	myapp:v1.2.3-go1.22.4-arm-v7    (ARM variant)
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
	// AppName is the image repository name for per-combination tags.
	AppName  string
	Registry string // optional registry prefix (e.g. ghcr.io/user)
	Tag      string // version tag for images (e.g. "v1.2.3"); empty → "latest"
	// BuildArgs are extra docker build arguments. Read-only during builds:
	// per-combination values (TARGETPLATFORM etc.) are merged into a local
	// copy so concurrent builds never mutate shared state.
	BuildArgs map[string]string
	Labels    map[string]string
	Debug     bool
	// CommandContext creates executed commands. Overridable for tests.
	CommandContext func(ctx context.Context, name string, args ...string) *exec.Cmd
}

func (d *DockerMatrixBuilder) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if d.CommandContext != nil {
		return d.CommandContext(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

// repoName returns the image repository, sanitized for Docker references.
func (d *DockerMatrixBuilder) repoName() string {
	if d.AppName != "" {
		return sanitizeNamePart(d.AppName)
	}
	return "phelix-build"
}

// tagOrDefault returns the user tag, or "latest" when unset.
func (d *DockerMatrixBuilder) tagOrDefault() string {
	if d.Tag != "" {
		return d.Tag
	}
	return "latest"
}

// DockerBuildConfig controls what the matrix Docker builder produces.
type DockerBuildConfig struct {
	MatrixTags   bool // produce per-combination tags
	MultiArchTag bool // produce a multi-arch manifest list tag
	Push         bool // push images after building
	PushPartial  bool // push only successful images even if some failed
}

// BuildDockerImage builds a Docker image for one combination. Each platform
// is built with `docker build --platform <platform>`; build-arg iteration is
// sorted so the command line is deterministic, and the per-combination
// TARGET* build args are merged into a copy — never written back into the
// builder's shared maps (which would race under concurrent builds).
func (d *DockerMatrixBuilder) BuildDockerImage(ctx context.Context, c Combination) *Result {
	result := &Result{Combination: c}
	start := time.Now()

	logLine := func(format string, args ...any) {
		if d.Debug {
			result.Log += fmt.Sprintf(format, args...) + "\n"
		}
	}

	// Docker containers only exist for linux — darwin/windows combinations
	// are binary-matrix territory, not image builds.
	if c.OS != "linux" {
		result.Status = "failed"
		result.Error = phelixerr.Newf(
			phelixerr.CodeInvalidArgument,
			"docker images cannot target %s — Docker supports linux platforms only; "+
				"use `phelix build --matrix` for native %s binaries", c.Platform, c.Platform)
		return result
	}

	// Build the per-combination image tag.
	imageName := fmt.Sprintf("%s:%s-%s", d.repoName(), d.tagOrDefault(), c.DockerTagSuffix())
	if d.Registry != "" {
		imageName = d.Registry + "/" + imageName
	}

	args := []string{"build", "-t", imageName}

	// Merge user build args with the per-combination TARGET* args into a
	// local copy. The builder's maps stay untouched (concurrency-safe).
	merged := make(map[string]string, len(d.BuildArgs)+4)
	for k, v := range d.BuildArgs {
		merged[k] = v
	}
	merged["TARGETPLATFORM"] = c.Platform
	merged["TARGETOS"] = c.OS
	merged["TARGETARCH"] = c.Arch
	if c.Variant != "" {
		merged["TARGETVARIANT"] = c.Variant
	}
	merged["TARGETVERSION"] = c.Version

	for _, k := range sortedKeys(merged) {
		args = append(args, "--build-arg", k+"="+merged[k])
	}
	for _, k := range sortedKeys(d.Labels) {
		args = append(args, "--label", k+"="+d.Labels[k])
	}

	args = append(args, "--platform", c.Platform)
	args = append(args, d.ProjectRoot)

	logLine("image:    %s", imageName)
	logLine("platform: %s", c.Platform)
	logLine("command:  docker %s", redactCommand(args))

	cmd := d.command(ctx, "docker", args...)

	output := &bytes.Buffer{}
	commandOutput := &progressOutputWriter{ctx: ctx, key: c.ID(), debug: d.Debug, log: output}
	ReportBuildProgress(ctx, c.ID(), "docker build", 0, 0)
	cmd.Stdout = commandOutput
	cmd.Stderr = commandOutput
	if err := cmd.Run(); err != nil {
		result.Status = "failed"
		// Keep the *exec.ExitError reachable; low-level docker output goes only
		// to the debug log, never into the structured error. Build args already
		// redacted above, so the command line in the log is safe.
		result.Error = phelixerr.Wrapf(phelixerr.CodeDocker, err, "docker build failed for %s", c.Platform)
		logLine("error: %v", err)
		if s := output.String(); s != "" {
			logLine("stderr: %s", phelixerr.Redact(s))
		}
		return result
	}

	result.Duration = time.Since(start)
	result.Status = "success"
	result.Artifact = imageName

	// Resolve the image's content digest (Docker's own sha256 of the built
	// image) so every matrix artifact — binary or image — carries a SHA-256.
	// A digest that cannot be resolved is an artifact-integrity failure: the
	// combination is failed rather than recorded with an unverifiable image.
	digest, derr := d.imageDigest(ctx, imageName)
	if derr != nil {
		result.Status = "failed"
		result.Artifact = imageName
		result.Error = phelixerr.Wrapf(phelixerr.CodeBuildFailed, derr,
			"artifact integrity check failed for %s — could not resolve the digest of %s", c.ID(), imageName)
		return result
	}
	result.SHA256 = digest
	return result
}

// imageDigest resolves a local image's content digest via
// `docker image inspect --format {{.Id}}`. The ID is Docker's sha256 content
// address of the image ("sha256:<64 hex>"); the bare hex digest is returned so
// it can live in the same sha256 field as binary checksums.
func (d *DockerMatrixBuilder) imageDigest(ctx context.Context, image string) (string, error) {
	cmd := d.command(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image)
	out, err := cmd.Output()
	if err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeDocker, err, "docker image inspect %s", image)
	}
	digest := strings.TrimPrefix(strings.TrimSpace(string(out)), "sha256:")
	if len(digest) != SHA256HexLen {
		return "", phelixerr.Newf(phelixerr.CodeDocker,
			"unexpected digest %q for image %s — expected a sha256 digest", digest, image)
	}
	return digest, nil
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

// BuildMultiArchManifest assembles a single multi-arch manifest list from the
// successfully-built per-combination images using `docker buildx imagetools
// create`, so one tag resolves to the correct platform at pull time.
func (d *DockerMatrixBuilder) BuildMultiArchManifest(ctx context.Context, results []Result, tag string) (*Result, error) {
	if len(results) == 0 {
		return nil, phelixerr.New(phelixerr.CodeDocker, "no images to create manifest from")
	}

	manifestTag := fmt.Sprintf("%s:%s", d.repoName(), tag)
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
	cmd := d.command(ctx, "docker", args...)

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
		msg := "build failed"
		if r.Error != nil {
			msg = phelixerr.Redact(r.Error.Error())
		}
		fmt.Fprintf(&b, "  - %s: %s\n", r.Combination.ID(), msg)
	}
	return b.String()
}

// BuildImage is a lower-level helper that builds a single Docker image
// without matrix context. Used by the non-matrix dockerize path.
func BuildImageSimple(ctx context.Context, projectRoot, imageName string, buildArgs map[string]string) (*Result, error) {
	start := time.Now()

	args := []string{"build", "-t", imageName}
	for _, k := range sortedKeys(buildArgs) {
		args = append(args, "--build-arg", k+"="+buildArgs[k])
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
