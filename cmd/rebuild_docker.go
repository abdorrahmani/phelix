package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/docker"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// dockerCmdRunner runs `docker <args...>` and returns captured stdout. It is a
// package var so cmd tests inject a fake, mirroring the dockerRunner seam in
// internal/deploy. The real runner streams stderr to the terminal (so a
// `docker compose build` shows progress) while capturing a bounded tail for the
// error message; parseable output (compose config json, docker ps) is on
// stdout, which is captured, never streamed.
var dockerCmdRunner = execDockerCmd

func execDockerCmd(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := cmd.Run(); err != nil {
		if detail := boundedTail(strings.TrimSpace(stderr.String()), 2048); detail != "" {
			return stdout.String(), phelixerr.Wrapf(phelixerr.CodeDocker, err, "docker %s: %s",
				strings.Join(args, " "), phelixerr.Redact(detail))
		}
		return stdout.String(), phelixerr.Wrapf(phelixerr.CodeDocker, err, "docker %s", strings.Join(args, " "))
	}
	return stdout.String(), nil
}

// boundedTail returns at most the last max bytes of s, so an error carries a
// useful slice of docker's stderr without dumping an entire build log.
func boundedTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

// dockerImageBuilder returns a deploy.DockerImageBuilder that builds the app's
// image for the docker runtime, dispatching on the resolved build strategy
// (deploy.docker.build). Both strategies tag the result <app>:v<version> so the
// rest of the pipeline (versions.json row, launcher, rollback) is byte-identical
// regardless of how the image was produced.
//
// Injecting the builder from cmd keeps internal/deploy free of any docker/compose
// knowledge (deploy stays runtime-agnostic; only this CLI wiring layer knows
// both). Language is detected from the build directory, exactly like dockerize.
func dockerImageBuilder(sourceDir, buildStrategy, composeFile, composeService string) deploy.DockerImageBuilder {
	return func(ctx context.Context, appName string, version int) (string, error) {
		if buildStrategy == project.DockerBuildCompose {
			return composeBuildImage(ctx, sourceDir, composeFile, composeService, appName, version)
		}
		return dockerfileBuildImage(sourceDir, appName, version)
	}
}

// dockerfileBuildImage is the default strategy — EXACTLY the pre-existing path:
// detect the language, ensure a Dockerfile (generated if none, a user's
// respected), and docker-build it as <app>:vN.
func dockerfileBuildImage(sourceDir, appName string, version int) (string, error) {
	lang, err := docker.DetectLanguage(sourceDir)
	if err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeUnsupportedProject, err,
			"docker runtime: detect language in %s", sourceDir)
	}

	// Generate Dockerfile/.dockerignore only when the project has none —
	// a user-provided Dockerfile is always respected (same rule as dockerize).
	if err := ensureDockerfileAndIgnore(sourceDir, lang); err != nil {
		return "", err
	}

	imageRef := fmt.Sprintf("%s:v%d", appName, version)
	if _, err := docker.BuildImage(docker.BuildConfig{
		ProjectRoot: sourceDir,
		ImageName:   imageRef,
		Labels: map[string]string{
			// Tie the image back to the managing agent's app, mirroring the
			// container labels the launcher sets at run time.
			deploy.DockerLabelApp: appName,
		},
	}); err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeDocker, err, "docker runtime: build image %s", imageRef)
	}
	return imageRef, nil
}

// composeBuildImage builds the image the way the named compose service would —
// compose applies the service's own build config (context/dockerfile/args/
// target) natively, so nothing here parses compose YAML — then tags the result
// <app>:vN for the rest of the pipeline.
//
// BUILD ONLY. The compose service's `environment`/`env_file` are NOT imported
// into the running container: runtime env stays with `phelix env` (encrypted,
// single source of truth). `depends_on` is not honored either — Phelix has no
// cross-app ordering, so the app must tolerate a briefly-unavailable backing
// service.
func composeBuildImage(ctx context.Context, sourceDir, composeFile, service, appName string, version int) (string, error) {
	if service == "" {
		return "", phelixerr.New(phelixerr.CodeConfiguration,
			"docker runtime: deploy.docker.build: compose requires deploy.docker.service")
	}
	file := composeFile
	if !filepath.IsAbs(file) {
		file = filepath.Join(sourceDir, file)
	}
	if _, err := os.Stat(file); err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeNotFound, err,
			"docker runtime: compose file %q not found for build: compose", file)
	}

	// Build the service using compose (its build block is applied natively).
	if _, err := dockerCmdRunner(ctx, "compose", "-f", file, "build", service); err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeDocker, err,
			"docker runtime: compose build of service %q in %s", service, file)
	}

	// Resolve the built image's name from the fully-resolved compose config.
	out, err := dockerCmdRunner(ctx, "compose", "-f", file, "config", "--format", "json")
	if err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeConfiguration, err,
			"docker runtime: read compose config %s", file)
	}
	built, err := composeServiceImage(out, service)
	if err != nil {
		return "", err
	}

	// Retag under Phelix's version scheme so launcher/rollback see <app>:vN.
	imageRef := fmt.Sprintf("%s:v%d", appName, version)
	if _, err := dockerCmdRunner(ctx, "tag", built, imageRef); err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeDocker, err,
			"docker runtime: tag compose image %s as %s", built, imageRef)
	}
	return imageRef, nil
}

// composeServiceImage extracts the built image reference for service from the
// JSON of `docker compose config`. A service with an explicit `image:` uses it;
// a build-only service is tagged by compose as "<project>-<service>", so that is
// reconstructed from the resolved project name. A pure function: no docker
// needed, so the dispatcher is unit-testable with canned config output.
func composeServiceImage(configJSON, service string) (string, error) {
	var cfg struct {
		Name     string `json:"name"`
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "docker runtime: parse compose config json")
	}
	svc, ok := cfg.Services[service]
	if !ok {
		return "", phelixerr.Newf(phelixerr.CodeNotFound,
			"docker runtime: compose service %q not found (available: %s)", service, composeServiceNames(cfg.Services))
	}
	if img := strings.TrimSpace(svc.Image); img != "" {
		return img, nil
	}
	if proj := strings.TrimSpace(cfg.Name); proj != "" {
		return proj + "-" + service, nil // compose's default name for a build-only service
	}
	return "", phelixerr.Newf(phelixerr.CodeConfiguration,
		"docker runtime: could not resolve the built image for compose service %q; add an explicit image: to it", service)
}

func composeServiceNames[T any](services map[string]T) string {
	names := make([]string, 0, len(services))
	for k := range services {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// warnIfComposeManaged warns — never fails — when the app also appears to run as
// a docker-compose service, i.e. the app tier would have two owners (duplicate
// instances / port conflicts). svc is the compose service name
// (deploy.docker.service under build: compose, else the app name as a
// heuristic). Containers carrying phelix.managed are ours and excluded. The
// compose-profile pattern runs no such container on the server, so this stays
// silent there — the desired signal. Best-effort: runner errors are ignored.
func warnIfComposeManaged(ctx context.Context, log deploy.Logger, appName, svc string) {
	if svc == "" || log == nil {
		return
	}
	out, err := dockerCmdRunner(ctx, "ps",
		"--filter", "label=com.docker.compose.service="+svc,
		"--filter", "status=running",
		"--format", `{{.ID}}|{{.Names}}|{{.Label "phelix.managed"}}`)
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) == 3 && strings.TrimSpace(parts[2]) == "true" {
			continue // a phelix-managed container — that's us, not a conflict
		}
		log.Warnf("app %q also appears to run as docker-compose service %q (container %s). "+
			"Phelix and compose would both manage it (duplicate instances / port conflicts). "+
			"Put that service under a compose profile, or `docker compose stop %s`, so only Phelix manages the app tier.",
			appName, svc, parts[0], svc)
		return
	}
}

// runDockerInitialBuild is the first-deploy path for a docker-runtime app.
//
// `phelix build` normally compiles a native binary and starts it as a classic
// host process — which is exactly wrong for a docker-runtime app (it would run
// a native process instead of a container, and classic is not even a valid
// docker strategy). So when deploy.runtime is docker, build registers the app
// and then hands off to the SAME zero-downtime engine `phelix rebuild` uses:
// image build → container start → health → proxy enrol. There is no active
// instance to displace on a brand-new app, so the proxy simply binds the
// public port. Subsequent deploys use `phelix rebuild` unchanged.
//
// The host Go/Rust toolchain is not required here — the image is built with the
// toolchain inside the container — so this checks for Docker instead.
func runDockerInitialBuild(cmd *cobra.Command, projCfg *project.Config, name string, publicPort int, currentDir string, lang builder.Language, noUpload bool) error {
	if err := docker.CheckDockerAvailable(); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeDockerDaemonUnavailable, err,
			"docker runtime: Docker is required to build and run %q as a container", name)
	}

	id := app.Manager.GenerateAppID()
	fmt.Printf("%s Building application %s (ID: %s) as a container\n",
		color.BlueString("→"), color.CyanString("'%s'", name), color.YellowString(id))
	fmt.Printf("  Language: %s\n", color.GreenString(string(lang)))

	if err := createAppEntry(id, name, lang, noUpload); err != nil {
		return err
	}
	if err := syncProjectResources(projCfg, id); err != nil {
		return err
	}
	if serr := syncProjectWatching(projCfg, id); serr != nil {
		fmt.Printf("  %s Warning: could not apply watching from %s: %v\n",
			color.YellowString("⚠"), project.FileName, serr)
	}
	if serr := syncProjectHealth(projCfg, id, name, publicPort); serr != nil {
		fmt.Printf("  %s Warning: could not apply health endpoints from %s: %v\n",
			color.YellowString("⚠"), project.FileName, serr)
	}

	// Resolve the deploy strategy from phelix.yaml into the shared rebuild
	// flags the deploy engine reads (build defines none of --blue-green /
	// --replicas / --strategy, so this falls through to deploy.strategy —
	// which validation guarantees is a zero-downtime one for docker).
	if err := applyConfigDeployStrategy(cmd, projCfg, name); err != nil {
		return err
	}
	rebuildArgs = buildArgs
	rebuildTag = buildTag

	appInfo, err := GetAppInfo(name)
	if err != nil {
		return err
	}
	return runZeroDowntimeDeploy(appInfo, name, publicPort, currentDir)
}
