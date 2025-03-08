package cmd

import (
	"fmt"
	"github.com/abdorrahmani/gophel/internal/auth"
	"github.com/abdorrahmani/gophel/internal/monitor"

	"github.com/spf13/cobra"
)

var AuthCmd = &cobra.Command{
	Use:   "auth",
	Short: "Authenticates user with gophel.anophel.com",
	Run: func(cmd *cobra.Command, args []string) {
		var username, token string
		fmt.Print("Enter username: ")
		fmt.Scanln(&username)
		fmt.Print("Enter token: ")
		fmt.Scanln(&token)

		if err := auth.Authenticate(username, token); err != nil {
			fmt.Println("Authentication failed:", err)
			return
		}
		fmt.Println("Authentication successful!")
		go monitor.StartMonitoring()
	},
}
