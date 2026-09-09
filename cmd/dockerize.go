package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/docker"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/matrix"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	dockerizeTag       string
	dockerizePush      bool
	dockerizeRegistry  string
	dockerizeBuildArgs []string
	dockerizeCompose   bool
	dockerizeDependsOn []string

	// Matrix dockerize flags.
	dockerizeMatrix       bool
	dockerizeGoVersions   []string
	dockerizeRustVersions []string
	dockerizePlatforms    []string
	dockerizeMatrixTags   bool
	dockerizeMultiArchTag bool
	dockerizePushPartial  bool
	dockerizeConcurrency  int
	dockerizeDebug        bool
)

var DockerizeCmd = &cobra.Command{
	Use:   "dockerize <AppName> [--tag v1.2.3] [--push] [--registry REGISTRY]",
	Short: "Build a Docker image for a Go or Rust application",
	Long: `Build a multi-stage Docker image for the project in the current directory.

Language is auto-detected from go.mod (Go) or Cargo.toml (Rust).
If a Dockerfile already exists, it is used as-is (never overwritten).
Otherwise, a multi-stage Dockerfile is generated with optimized layer caching.

The --with-compose flag generates a docker-compose.yml with optional sidecar
services (--depends-on redis,postgres). Phelix does NOT manage the lifecycle
of compose-defined services — use 'docker compose up -d' directly.

Registry credentials can be stored securely using the same AES-256-GCM
encryption mechanism as environment variables (via the internal/env package).`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "missing required <AppName>; usage: phelix dockerize <AppName> [--tag v1.2.3] [--push] [--registry REGISTRY]")
			}
			chosen, err := PromptApp(false, "Select application to dockerize")
			if err != nil {
				return err
			}
			args = []string{chosen}
		}

		name := args[0]
		if err := validateName(name); err != nil {
			return err
		}

		if IsInteractive() {
			if !cmd.Flags().Changed("tag") {
				if tag, err := PromptString("Image tag (leave blank for none)", ""); err == nil {
					dockerizeTag = tag
				}
			}
			if !cmd.Flags().Changed("push") {
				if push, err := PromptConfirm("Push the image to a registry after building?", dockerizePush); err == nil {
					dockerizePush = push
				}
			}
			if dockerizePush && dockerizeRegistry == "" {
				if reg, err := PromptString("Registry prefix (e.g. ghcr.io/user)", ""); err == nil {
					dockerizeRegistry = reg
				}
			}
		}

		// Load state
		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
		}

		// Check Docker availability
		if err := docker.CheckDockerAvailable(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeDockerDaemonUnavailable, "docker is not available", err)
		}

		// Get project root
		currentDir, err := os.Getwd()
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to get current directory", err)
		}

		// Detect language
		lang, err := docker.DetectLanguage(currentDir)
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeUnsupportedProject, "could not detect project language", err)
		}

		// --- Matrix dockerize path ---
		if matrix.IsMatrixMode(dockerizeMatrix, dockerizeGoVersions, dockerizeRustVersions, dockerizePlatforms) {
			return runDockerizeMatrixMode(name, string(lang), currentDir, dockerizeTag, dockerizeRegistry, dockerizePush, dockerizePushPartial)
		}

		fmt.Printf("%s Dockerizing application %s (%s)\n",
			color.BlueString("→"), color.CyanString("'%s'", name), color.GreenString(string(lang)))

		// --- Dockerfile generation ---
		gen := docker.NewDockerfileGenerator(currentDir, lang)
		if gen.HasExistingDockerfile() {
			fmt.Printf("  %s Using existing Dockerfile\n", color.YellowString("Note:"))
		} else {
			fmt.Printf("  %s Generating multi-stage Dockerfile for %s...\n", color.BlueString("→"), color.GreenString(string(lang)))
			if err := gen.WriteDockerfile(); err != nil {
				return phelixerr.Wrap(phelixerr.CodeDocker, "failed to generate Dockerfile", err)
			}
			fmt.Printf("  %s Dockerfile created\n", color.GreenString("✓"))
		}

		// --- .dockerignore generation ---
		if docker.HasExistingDockerignore(currentDir) {
			fmt.Printf("  %s Using existing .dockerignore\n", color.YellowString("Note:"))
		} else {
			fmt.Printf("  %s Generating .dockerignore...\n", color.BlueString("→"))
			if err := docker.WriteDockerignore(currentDir); err != nil {
				return phelixerr.Wrap(phelixerr.CodeDocker, "failed to generate .dockerignore", err)
			}
			fmt.Printf("  %s .dockerignore created\n", color.GreenString("✓"))
		}

		// --- Build image ---
		imageName := name
		if dockerizeTag != "" {
			imageName = name + ":" + dockerizeTag
		} else {
			imageName = name + ":latest"
		}

		// If registry is specified, prefix the image name
		if dockerizeRegistry != "" {
			imageName = dockerizeRegistry + "/" + imageName
		}

		// Parse build args into a map (malformed entries fail fast).
		buildArgsMap, err := parseDockerBuildArgs(dockerizeBuildArgs)
		if err != nil {
			return err
		}

		// OCI-standard labels
		gitCommit := deploy.DetectGitCommit(currentDir)
		labels := map[string]string{
			"org.opencontainers.image.version":  dockerizeTag,
			"org.opencontainers.image.revision": gitCommit,
			"org.opencontainers.image.created":  fmt.Sprintf("%d", os.Getpid()), // placeholder; real timestamp set by Docker
		}

		fmt.Printf("  %s Building Docker image %s...\n", color.BlueString("→"), color.CyanString(imageName))

		buildResult, err := docker.BuildImage(docker.BuildConfig{
			ProjectRoot: currentDir,
			ImageName:   imageName,
			BuildArgs:   buildArgsMap,
			Labels:      labels,
		})
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeDocker, "docker image build failed", err)
		}

		fmt.Printf("  %s Image built in %s\n", color.GreenString("✓"), color.YellowString(buildResult.Duration.String()))

		// --- Record version in the versioning system ---
		// Store the Docker image reference so future rollback logic can use
		// "docker run <image>" as a BuildSource implementation.
		logger := &colorLogger{}
		_, verErr := deploy.RecordDockerBuild(
			name, imageName, dockerizeTag, gitCommit,
			deploy.DefaultRetention{Max: 5}, logger,
		)
		if verErr != nil {
			fmt.Printf("  %s Warning: could not record version: %v\n", color.YellowString("⚠"), verErr)
		} else {
			fmt.Printf("  %s Docker image recorded in version history\n", color.GreenString("✓"))
		}

		// --- Push to registry ---
		if dockerizePush {
			fmt.Printf("  %s Pushing to registry...\n", color.BlueString("→"))
			if err := docker.PushImage(docker.PushConfig{
				ImageName: imageName,
				Registry:  dockerizeRegistry,
			}); err != nil {
				return phelixerr.Wrap(phelixerr.CodeDocker, "docker image push failed", err)
			}
			fmt.Printf("  %s Pushed successfully\n", color.GreenString("✓"))
		}

		// --- Generate docker-compose.yml ---
		if dockerizeCompose {
			if docker.HasExistingCompose(currentDir) {
				fmt.Printf("  %s Using existing docker-compose.yml\n", color.YellowString("Note:"))
			} else {
				fmt.Printf("  %s Generating docker-compose.yml...\n", color.BlueString("→"))
				// Determine app port (default 8080)
				appPort := defaultPort
				if appInfo, err := GetAppInfo(name); err == nil && appInfo.Port != 0 {
					appPort = appInfo.Port
				}
				if err := docker.WriteComposeFile(currentDir, docker.ComposeConfig{
					AppName:   name,
					ImageName: imageName,
					AppPort:   appPort,
					DependsOn: dockerizeDependsOn,
				}); err != nil {
					return phelixerr.Wrap(phelixerr.CodeDocker, "failed to generate docker-compose.yml", err)
				}
				fmt.Printf("  %s docker-compose.yml created\n", color.GreenString("✓"))
				fmt.Printf("  %s Phelix does NOT manage compose-defined services. Use: docker compose up -d\n",
					color.YellowString("⚠"))
			}
		}

		fmt.Printf("\n%s Docker image ready: %s\n", color.GreenString("✓"), color.CyanString(imageName))

		// Report dockerize event
		phelixgrpc.ReportEvent("", name, "dockerize", true, "", 0, "docker", dockerizeTag)
		return nil
	},
}

func init() {
	DockerizeCmd.Flags().StringVar(&dockerizeTag, "tag", "", "Version tag for the Docker image (e.g. v1.2.3)")
	DockerizeCmd.Flags().BoolVar(&dockerizePush, "push", false, "Push the image to the registry after building")
	DockerizeCmd.Flags().StringVar(&dockerizeRegistry, "registry", "", "Registry prefix (e.g. ghcr.io/user, docker.io/myorg)")
	DockerizeCmd.Flags().StringArrayVarP(&dockerizeBuildArgs, "build-arg", "a", nil, "Extra build argument KEY=value (repeatable)")
	DockerizeCmd.Flags().BoolVar(&dockerizeCompose, "with-compose", false, "Generate docker-compose.yml with the app service")
	DockerizeCmd.Flags().StringSliceVar(&dockerizeDependsOn, "depends-on", nil, "Sidecar services for compose (redis, postgres, mysql, mongodb, rabbitmq)")

	// Matrix dockerize flags.
	DockerizeCmd.Flags().BoolVar(&dockerizeMatrix, "matrix", false, "Enable matrix mode: build Docker images for multiple version × platform combinations")
	DockerizeCmd.Flags().StringSliceVar(&dockerizeGoVersions, "go-versions", nil, "Go versions to build with (e.g. 1.22,1.23)")
	DockerizeCmd.Flags().StringSliceVar(&dockerizeRustVersions, "rust-versions", nil, "Rust versions to build with (e.g. 1.77,1.78)")
	DockerizeCmd.Flags().StringSliceVar(&dockerizePlatforms, "platforms", nil, "Target platforms (e.g. linux/amd64,linux/arm64)")
	DockerizeCmd.Flags().BoolVar(&dockerizeMatrixTags, "matrix-tags", false, "Tag each version×platform combination separately (e.g. myapp:go1.22-linux-amd64)")
	DockerizeCmd.Flags().BoolVar(&dockerizeMultiArchTag, "multi-arch-tag", false, "Create a multi-arch manifest list tag via docker buildx")
	DockerizeCmd.Flags().BoolVar(&dockerizePushPartial, "push-partial", false, "Push only successful images even if some combinations failed")
	DockerizeCmd.Flags().IntVar(&dockerizeConcurrency, "matrix-concurrency", matrix.DefaultConcurrency, "Max parallel builds in matrix mode")
	DockerizeCmd.Flags().BoolVar(&dockerizeDebug, "debug", false, "Show verbose build output, commands, and Docker operations")
}

// runDockerizeMatrixMode builds Docker images for a matrix of version × platform
// combinations. Each combination gets its own image tag
// ({app}:{tag|latest}-{lang}{version}-{arch}[-{variant}]); with
// --multi-arch-tag, the per-version platform images are additionally assembled
// into one multi-arch manifest list per toolchain version.
//
// Push semantics are fail-closed by default: if any combination failed to build,
// we refuse to push any image. This prevents publishing a partial/inconsistent
// release where some platforms work and others don't. The --push-partial flag
// overrides this behavior.
func runDockerizeMatrixMode(name string, lang string, projectRoot, tag, registry string, push, pushPartial bool) error {
	versions := dockerizeGoVersions
	langEnum := builder.Go
	if lang == "rust" {
		versions = dockerizeRustVersions
		langEnum = builder.Rust
	}
	if len(versions) == 0 {
		return phelixerr.Newf(
			phelixerr.CodeInvalidArgument,
			"matrix mode requires version flags: use --go-versions or --rust-versions",
		)
	}

	plan, err := matrix.ParsePlan(langEnum, versions, dockerizePlatforms)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeInvalidArgument, "invalid matrix build plan", err)
	}

	buildArgs, err := parseDockerBuildArgs(dockerizeBuildArgs)
	if err != nil {
		return err
	}

	fmt.Printf("%s Docker matrix build: %s (%d combinations)\n",
		color.BlueString("→"), color.CyanString(name), len(plan.Combinations))
	for _, c := range plan.Combinations {
		fmt.Printf("    %s %s\n", color.New(color.Faint).Sprint("•"), c.ID())
	}
	fmt.Println()

	// Ensure buildx is available for multi-arch builds.
	if err := matrix.EnsureBuildx(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeDocker, "docker buildx is required for this build", err)
	}

	dmb := &matrix.DockerMatrixBuilder{
		ProjectRoot: projectRoot,
		AppName:     name,
		Registry:    registry,
		Tag:         tag,
		BuildArgs:   buildArgs,
		Debug:       dockerizeDebug,
	}

	// Build each combination.
	startTime := time.Now()
	results := matrix.Execute(plan, dmb.BuildDockerImage, matrix.ExecutorConfig{
		Concurrency: dockerizeConcurrency,
		Debug:       dockerizeDebug,
	})

	// Generate report.
	report := matrix.GenerateReport(name, results, startTime)
	report.PrintTerminal()

	reportPath, _ := report.WriteJSON(projectRoot)
	if reportPath != "" {
		fmt.Printf("  %s Report written to %s\n", color.GreenString("✓"), reportPath)
	}

	// Handle push with fail-closed semantics.
	if push {
		if err := dmb.PushImages(results, pushPartial); err != nil {
			return phelixerr.Wrap(phelixerr.CodeDocker, "docker image push failed", err)
		}
		fmt.Printf("  %s All images pushed\n", color.GreenString("✓"))
	}

	// Assemble multi-arch manifest lists (one per toolchain version) when
	// requested. This runs after the push so the source images already exist
	// in the registry when the manifest is created.
	var multiArchImages []string
	if dockerizeMultiArchTag {
		byVersion, order := groupSuccessByVersion(results)
		for _, ver := range order {
			manifestTag := matrixManifestTag(tag, ver, len(plan.Combinations))
			manifest, err := dmb.BuildMultiArchManifest(context.Background(), byVersion[ver], manifestTag)
			if err != nil {
				return phelixerr.Wrap(phelixerr.CodeDocker, "failed to create multi-arch manifest", err)
			}
			multiArchImages = append(multiArchImages, manifest.Artifact)
			fmt.Printf("  %s Multi-arch manifest created: %s\n", color.GreenString("✓"), manifest.Artifact)
		}
	}

	// Record successful artifacts in the versioning system.
	if report.Succeeded > 0 {
		artifacts := make([]deploy.MatrixArtifact, 0, report.Succeeded)
		for _, r := range results {
			if r.Status != "success" {
				continue
			}
			artifacts = append(artifacts, deploy.MatrixArtifact{
				Platform: r.Combination.Platform,
				Version:  r.Combination.Version,
				ImageTag: r.Artifact,
				Status:   r.Status,
			})
		}

		gitCommit := deploy.DetectGitCommit(projectRoot)
		logger := &colorLogger{}
		_, verErr := deploy.RecordMatrixBuild(
			name, tag, gitCommit, artifacts, strings.Join(multiArchImages, ", "),
			deploy.DefaultRetention{Max: 5}, logger,
		)
		if verErr != nil {
			fmt.Printf("  %s Warning: could not record version: %v\n", color.YellowString("⚠"), verErr)
		} else {
			fmt.Printf("  %s Docker matrix build recorded in version history\n", color.GreenString("✓"))
		}
	}

	if report.Failed > 0 {
		return phelixerr.Newf(
			phelixerr.CodeBuildFailed,
			"matrix dockerize completed with %d failure(s) out of %d combinations",
			report.Failed, report.Total,
		)
	}

	return nil
}

// parseDockerBuildArgs converts --build-arg KEY=VALUE flag values into a map,
// rejecting malformed entries (missing "=" or empty key) instead of silently
// dropping them.
func parseDockerBuildArgs(args []string) (map[string]string, error) {
	out := make(map[string]string, len(args))
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if !ok || key == "" {
			return nil, phelixerr.Newf(
				phelixerr.CodeInvalidArgument,
				"invalid --build-arg %q (expected KEY=VALUE)", arg)
		}
		out[key] = value
	}
	return out, nil
}

// matrixManifestTag derives the manifest-list tag from the user-supplied base
// tag. A single combination keeps the base tag (or "latest"); multiple
// combinations qualify it with the toolchain version so concurrent versions
// never overwrite each other's manifest (e.g. "v2-1.23", "latest-1.23").
func matrixManifestTag(base, version string, count int) string {
	if base == "" {
		base = "latest"
	}
	if count <= 1 {
		return base
	}
	return base + "-" + strings.TrimPrefix(version, "go")
}

// groupSuccessByVersion buckets successful results by toolchain version,
// preserving first-seen version order.
func groupSuccessByVersion(results []matrix.Result) (map[string][]matrix.Result, []string) {
	byVersion := make(map[string][]matrix.Result)
	var order []string
	for _, r := range results {
		if r.Status != "success" || r.Artifact == "" {
			continue
		}
		ver := r.Combination.Version
		if _, seen := byVersion[ver]; !seen {
			order = append(order, ver)
		}
		byVersion[ver] = append(byVersion[ver], r)
	}
	return byVersion, order
}
