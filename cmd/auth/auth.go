package auth

import (
	"github.com/spf13/cobra"
)

// Cmd is the root command for authentication-related actions.
var Cmd = &cobra.Command{
	Use:   "auth",
	Short: "Authenticate with phelix.anophel.com",
}

func init() {
	setupLogging()

	// Register subcommands
	Cmd.AddCommand(LoginCmd)
	Cmd.AddCommand(StatusCmd)
	Cmd.AddCommand(LogoutCmd)
}
