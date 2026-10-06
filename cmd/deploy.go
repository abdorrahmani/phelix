package cmd

import (
	"fmt"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var deployUnlockJSON bool

var DeployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Manage deployment state",
}

var deployUnlockCmd = &cobra.Command{
	Use:   "unlock [AppName]",
	Short: "Clear a stale deploy lock for an application",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Machine mode: progress detours to stderr for the whole command.
		if deployUnlockJSON {
			restore := machine.EnterJSON()
			defer restore()
		}

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

		// deploy unlock is lock management, not a deployment execution: it has
		// no operation identity of its own (nothing durable is minted), and it
		// is naturally repeatable — clearing an absent lock reports so.
		if state.OpLock == nil {
			fmt.Printf("%s No deploy lock held for %s\n", color.GreenString("✓"), color.CyanString("'%s'", appName))
			return writeEnvelopeResult(machine.Success("", deployUnlockResult{App: appName, Unlocked: false}))
		}

		fmt.Printf("  Clearing stale lock: %s (pid %d, since %s)\n",
			state.OpLock.Operation, state.OpLock.PID,
			state.OpLock.StartedAt.Format("2006-01-02 15:04:05"))

		state.OpLock = nil
		if err := deploy.Store(state); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to save deploy state", err)
		}

		fmt.Printf("%s Deploy lock cleared for %s\n", color.GreenString("✓"), color.CyanString("'%s'", appName))
		return writeEnvelopeResult(machine.Success("", deployUnlockResult{App: appName, Unlocked: true}))
	},
}

func init() {
	deployUnlockCmd.Flags().BoolVar(&deployUnlockJSON, "json", false,
		"Output machine-readable JSON (stdout carries only the result envelope; progress moves to stderr)")
	DeployCmd.AddCommand(deployUnlockCmd)
}
