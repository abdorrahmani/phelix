package cmd

import (
	"github.com/abdorrahmani/phelix/cmd/auth"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/spf13/cobra"
)

var WizardCmd = &cobra.Command{
	Use:   "wizard",
	Short: "Interactive step-by-step menu for any Phelix task",
	Long: `Launch an interactive, survey-driven menu that walks you through any
operator task (build, rebuild, rollback, lifecycle, env, health, dockerize,
deploy, auth, …). Each choice routes to the command's own logic, so the wizard
and the plain subcommands share identical behavior.

This command is fully interactive — it does nothing when stdin is not a TTY.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !IsInteractive() {
			return phelixerr.Newf(
				phelixerr.CodeInvalidArgument,
				"the wizard requires an interactive terminal — run a specific subcommand instead (e.g. 'phelix build <NAME>')",
			)
		}

		tasks := []string{
			"Build a new application",
			"Rebuild an application",
			"Roll back an application",
			"Start an application",
			"Stop an application",
			"Restart an application",
			"Show application status",
			"Show application logs",
			"List all applications",
			"Manage environment variables",
			"Configure health checks",
			"Dockerize an application",
			"Unlock a stuck deploy",
			"Log in to Phelix",
			"Show version",
		}

		chosen, err := PromptSelect("What would you like to do?", tasks)
		if err != nil {
			return err
		}

		switch chosen {
		case "Build a new application":
			return BuildCmd.RunE(BuildCmd, []string{})
		case "Rebuild an application":
			return RebuildCmd.RunE(RebuildCmd, []string{})
		case "Roll back an application":
			return RollbackCmd.RunE(RollbackCmd, []string{})
		case "Start an application":
			return StartCmd.RunE(StartCmd, []string{})
		case "Stop an application":
			return StopCmd.RunE(StopCmd, []string{})
		case "Restart an application":
			return RestartCmd.RunE(RestartCmd, []string{})
		case "Show application status":
			return StatusCmd.RunE(StatusCmd, []string{})
		case "Show application logs":
			name, perr := PromptApp(false, "Select application to view logs")
			if perr != nil {
				return perr
			}
			return LogCmd.RunE(LogCmd, []string{name})
		case "List all applications":
			return ListCmd.RunE(ListCmd, []string{})
		case "Manage environment variables":
			// Let the env command prompt for the sub-action, app, and key.
			return EnvCmd.RunE(EnvCmd, []string{})
		case "Configure health checks":
			return wizardHealth()
		case "Dockerize an application":
			return DockerizeCmd.RunE(DockerizeCmd, []string{})
		case "Unlock a stuck deploy":
			return deployUnlockCmd.RunE(deployUnlockCmd, []string{})
		case "Log in to Phelix":
			return auth.LoginCmd.RunE(auth.LoginCmd, []string{})
		case "Show version":
			return VersionCmd.RunE(VersionCmd, []string{})
		default:
			return phelixerr.Newf(phelixerr.CodeInvalidArgument, "unknown task: %s", chosen)
		}
	},
}

// wizardHealth routes to one of the health sub-commands, reusing their own
// interactive app/flag prompts.
func wizardHealth() error {
	action, err := PromptSelect(
		"Which health action?",
		[]string{"set", "add", "remove", "list", "status"},
	)
	if err != nil {
		return err
	}
	switch action {
	case "set":
		return healthSetCmd.RunE(healthSetCmd, []string{})
	case "add":
		return healthAddCmd.RunE(healthAddCmd, []string{})
	case "remove":
		return healthRemoveCmd.RunE(healthRemoveCmd, []string{})
	case "list":
		return healthListCmd.RunE(healthListCmd, []string{})
	case "status":
		return healthStatusCmd.RunE(healthStatusCmd, []string{})
	default:
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "unknown health action: %s", action)
	}
}
