package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/docker"
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
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if err := validateName(name); err != nil {
			return err
		}

		// Load state
		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("%s Failed to load state: %w", color.RedString("✗"), err)
		}

		// Check Docker availability
		if err := docker.CheckDockerAvailable(); err != nil {
			return fmt.Errorf("%s %v", color.RedString("✗"), err)
		}

		// Get project root
		currentDir, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("%s failed to get current directory: %v", color.RedString("✗"), err)
		}

		// Detect language
		lang, err := docker.DetectLanguage(currentDir)
		if err != nil {
			return fmt.Errorf("%s %v", color.RedString("✗"), err)
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
				return fmt.Errorf("%s failed to generate Dockerfile: %v", color.RedString("✗"), err)
			}
			fmt.Printf("  %s Dockerfile created\n", color.GreenString("✓"))
		}

		// --- .dockerignore generation ---
		if docker.HasExistingDockerignore(currentDir) {
			fmt.Printf("  %s Using existing .dockerignore\n", color.YellowString("Note:"))
		} else {
			fmt.Printf("  %s Generating .dockerignore...\n", color.BlueString("→"))
			if err := docker.WriteDockerignore(currentDir); err != nil {
				return fmt.Errorf("%s failed to generate .dockerignore: %v", color.RedString("✗"), err)
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

		// Parse build args into a map
		buildArgsMap := make(map[string]string)
		for _, arg := range dockerizeBuildArgs {
			parts := strings.SplitN(arg, "=", 2)
			if len(parts) == 2 {
				buildArgsMap[parts[0]] = parts[1]
			}
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
			return fmt.Errorf("%s %v", color.RedString("✗"), err)
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
				return fmt.Errorf("%s %v", color.RedString("✗"), err)
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
					return fmt.Errorf("%s failed to generate docker-compose.yml: %v", color.RedString("✗"), err)
				}
				fmt.Printf("  %s docker-compose.yml created\n", color.GreenString("✓"))
				fmt.Printf("  %s Phelix does NOT manage compose-defined services. Use: docker compose up -d\n",
					color.YellowString("⚠"))
			}
		}

		fmt.Printf("\n%s Docker image ready: %s\n", color.GreenString("✓"), color.CyanString(imageName))
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
}
