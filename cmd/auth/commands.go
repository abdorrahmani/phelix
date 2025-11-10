package auth

import (
	"fmt"
	"log"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// LoginCmd handles the "gophel auth" login command.
var LoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate user with Gophel",
	RunE: func(cmd *cobra.Command, args []string) error {
		var username, apiKey string
		fmt.Print("Enter username: ")
		fmt.Scanln(&username)
		fmt.Print("Enter API Key: ")
		fmt.Scanln(&apiKey)

		if err := authenticate(username, apiKey); err != nil {
			log.Printf("Authentication failed: %v", err)
			return err
		}

		printWelcome(username)
		return nil
	},
}

// StatusCmd prints session status.
var StatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Shows authentication status",
	RunE: func(cmd *cobra.Command, args []string) error {
		session, err := GetValidSession()
		if err != nil {
			return err
		}

		if err := VerifySession(session); err != nil {
			return fmt.Errorf("session invalid: %w", err)
		}

		printSession(session)
		return nil
	},
}

// LogoutCmd logs out the current session.
var LogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Logout from Gophel",
	RunE: func(cmd *cobra.Command, args []string) error {
		session, err := GetValidSession()
		if err != nil {
			fmt.Println("No active session found.")
			return nil
		}

		if err := performLogout(session); err != nil {
			return err
		}
		if err := removeSession(); err != nil {
			return err
		}
		fmt.Println("Successfully logged out.")
		return nil
	},
}

func printWelcome(username string) {
	green := color.New(color.FgGreen).SprintFunc()
	bold := color.New(color.Bold).SprintFunc()
	fmt.Printf("\nHi %s, welcome to Gophel!\n", bold(username))
	fmt.Printf("You can monitor your apps at %s\n", green("gophel.anophel.com"))
	fmt.Printf("%s\n\n", green("Authentication successful!"))
	log.Printf("Authentication successful for %s", username)
}

func printSession(session *Session) {
	green := color.New(color.FgGreen).SprintFunc()
	fmt.Printf("Authenticated as: %s\n", green(session.SessionID))
	fmt.Printf("Token: %s\n", session.Token)
	fmt.Printf("Expires at: %s\n", session.ExpiresAt.Format(time.RFC1123))
}
