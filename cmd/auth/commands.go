package auth

import (
	"fmt"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	username string
	apiKey   string
)

// LoginCmd handles the "phelix auth" login command.
var LoginCmd = &cobra.Command{
	Use:   "login --username <username> --apikey <apikey>",
	Short: "Authenticate user with Phelix",
	RunE: func(cmd *cobra.Command, args []string) error {
		if username == "" && apiKey == "" {
			fmt.Print("Enter username: ")
			fmt.Scanln(&username)
			fmt.Print("Enter API Key: ")
			fmt.Scanln(&apiKey)
		}

		if err := authenticate(username, apiKey); err != nil {
			return phelixerr.Wrapf(
				phelixerr.CodeInvalidCredentials,
				err,
				"authentication failed for username %q",
				username,
			)
		}

		printWelcome(username)

		// Now that a session exists, upload all locally-managed apps so apps
		// created while logged out start appearing on the dashboard immediately.
		if err := app.Manager.LoadState(); err != nil {
			fmt.Printf("  %s Could not load local app state: %v\n", color.YellowString("⚠"), err)
		} else {
			count := len(app.Manager.ListApplications())
			if count == 0 {
				fmt.Printf("  %s No local apps to sync — they'll be uploaded as you build them\n", color.BlueString("→"))
			} else {
				fmt.Printf("  %s Syncing %d existing app(s) to the dashboard...\n", color.BlueString("→"), count)
				if err := SendAppsToServer(); err != nil {
					fmt.Printf("  %s Could not sync apps to dashboard: %v\n", color.YellowString("⚠"), err)
				} else {
					fmt.Printf("  %s Synced %d app(s) to the dashboard\n", color.GreenString("✓"), count)
				}
			}
		}

		return nil
	},
}

func init() {
	LoginCmd.Flags().StringVarP(&username, "username", "u", "", "Username to login")
	LoginCmd.Flags().StringVarP(&apiKey, "apiKey", "k", "", "api key to login")
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

		status, err := VerifySession(session)
		if err != nil {
			return phelixerr.Wrapf(
				phelixerr.CodeUnauthenticated,
				err,
				"session invalid",
			)
		}

		printSession(session, status)
		return nil
	},
}

// LogoutCmd logs out the current session.
var LogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Logout from Phelix",
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
		fmt.Println("✓ Successfully logged out.")
		return nil
	},
}

func printWelcome(username string) {
	green := color.New(color.FgGreen).SprintFunc()
	bold := color.New(color.Bold).SprintFunc()
	fmt.Printf("\n%s\n", cyan.Sprintf(
		"██████╗ ██╗  ██╗███████╗██╗     ██╗██╗  ██╗\n"+
			"██╔══██╗██║  ██║██╔════╝██║     ██║╚██╗██╔╝\n"+
			"██████╔╝███████║█████╗  ██║     ██║ ╚███╔╝ \n"+
			"██╔═══╝ ██╔══██║██╔══╝  ██║     ██║ ██╔██╗ \n"+
			"██║     ██║  ██╗███████╗███████╗██║██╔╝ ██╗\n"+
			"╚═╝     ╚═╝  ╚═╝╚══════╝╚══════╝╚═╝╚═╝  ╚═╝",
	))
	fmt.Printf("Hi %s, welcome to Phelix!\n", bold(username))
	fmt.Printf("You can monitor your apps at %s\n", green("phelix.anophel.com"))
	fmt.Printf("%s\n\n", green("Authentication successful!"))
}

// cyan color func for the banner.
var cyan = color.New(color.FgCyan)

func printSession(session *Session, status *SessionStatus) {
	green := color.New(color.FgGreen).SprintFunc()
	who := session.Username
	if who == "" && status != nil && status.User != "" {
		who = status.User
	}
	if who == "" {
		who = session.SessionID
	}
	fmt.Printf("• Authenticated as: %s\n", green(who))
	if status != nil {
		fmt.Printf("• Session: %s\n", status.SessionID)
		fmt.Printf("• CLI: %s (%s/%s)\n", status.CliVersion, status.OS, status.Arch)
	}
	fmt.Printf("• Expires at: %s\n", session.ExpiresAt.Format(time.RFC1123))
}
