package cmd

import (
	"fmt"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var DeployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Manage deployment state",
}

var deployUnlockCmd = &cobra.Command{
	Use:   "unlock [AppName]",
	Short: "Clear a stale deploy lock for an application",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "missing required <AppName>; usage: phelix deploy unlock <AppName>")
			}
			chosen, err := PromptApp(false, "Select application to unlock")
			if err != nil {
				return err
			}
			args = []string{chosen}
		}

		appName := args[0]

		state, err := deploy.Load(appName)
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeDeployFailed, "could not load deploy state", err)
		}

		if state.OpLock == nil {
			fmt.Printf("%s No deploy lock held for %s\n", color.GreenString("✓"), color.CyanString("'%s'", appName))
			return nil
		}

		fmt.Printf("  Clearing stale lock: %s (pid %d, since %s)\n",
			state.OpLock.Operation, state.OpLock.PID,
			state.OpLock.StartedAt.Format("2006-01-02 15:04:05"))

		state.OpLock = nil
		if err := deploy.Store(state); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to save deploy state", err)
		}

		fmt.Printf("%s Deploy lock cleared for %s\n", color.GreenString("✓"), color.CyanString("'%s'", appName))
		return nil
	},
}

func init() {
	DeployCmd.AddCommand(deployUnlockCmd)
}
