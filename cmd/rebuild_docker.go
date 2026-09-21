package cmd

import (
	"context"
	"fmt"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/docker"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// dockerImageBuilder returns a deploy.DockerImageBuilder that builds the app's
// image for the docker runtime. It reuses the same Dockerfile generation and
// docker-build path as `phelix dockerize`, so an app deploys through containers
// with the exact multi-stage image the user would get manually. The image is
// tagged <app>:v<version> so it lines up with the versions.json row the
// DockerBuildSource records; the launcher runs that ref.
//
// Injecting the builder from cmd keeps internal/deploy free of an import on
// internal/docker (deploy stays runtime-agnostic; only the CLI wiring knows
// both). Language is detected from the build directory, exactly like dockerize.
func dockerImageBuilder(sourceDir string) deploy.DockerImageBuilder {
	return func(_ context.Context, appName string, version int) (string, error) {
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
