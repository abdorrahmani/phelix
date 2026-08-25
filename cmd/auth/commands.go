package auth

import (
	"fmt"
	"os"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/connstate"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/AlecAivazis/survey/v2"
	"golang.org/x/term"
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
			if term.IsTerminal(int(os.Stdin.Fd())) {
				if err := survey.AskOne(&survey.Input{Message: "Enter username"}, &username); err != nil {
					return phelixerr.Wrap(phelixerr.CodeInvalidArgument, "input cancelled", err)
				}
				if err := survey.AskOne(&survey.Password{Message: "Enter API Key"}, &apiKey); err != nil {
					return phelixerr.Wrap(phelixerr.CodeInvalidArgument, "input cancelled", err)
				}
			} else {
				fmt.Print("Enter username: ")
				fmt.Scanln(&username)
				fmt.Print("Enter API Key: ")
				fmt.Scanln(&apiKey)
			}
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
		// A successful login restores authenticated sync (possibly after an
		// auth_expired pause) — record it so 'phelix auth status' and the next
		// metadata sync report the recovered state.
		connstate.MarkConnected()

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
		printConnectionState()
		return nil
	},
}

// printConnectionState renders the backend connection/auth state, separate
// from the session validity above and from the local runtime status.
func printConnectionState() {
	state := connstate.Get()
	if state == "" {
		state = connstate.Connected
	}
	switch state {
	case connstate.Connected:
		fmt.Printf("• Dashboard connection: %s\n", color.GreenString(connstate.Connected))
	case connstate.AuthExpired:
		fmt.Printf("• Dashboard connection: %s (run 'phelix auth login' to restore)\n",
			color.YellowString(connstate.AuthExpired))
	case connstate.Disconnected:
		fmt.Printf("• Dashboard connection: %s\n", color.HiBlackString(connstate.Disconnected))
	default:
		fmt.Printf("• Dashboard connection: %s\n", state)
	}
}

// LogoutCmd logs out the current session.
//
// Logout only severs the dashboard connection: it notifies the backend (so
// the agent is marked disconnected immediately, not after a heartbeat
// timeout), closes the gRPC client, and deletes the local session file. It
// does NOT stop the monitor daemon, managed apps, or delete the persistent
// agent_id — local lifecycle commands keep working, and 'phelix auth login'
// later reattaches the same agent record on the dashboard.
var LogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Logout from Phelix (dashboard sync stops; local apps and monitor keep running)",
	RunE: func(cmd *cobra.Command, args []string) error {
		session, err := GetValidSession()
		if err != nil {
			fmt.Println("No active session found.")
			return nil
		}

		// Tell the backend BEFORE deleting the local session — the notification
		// needs the session credentials to authenticate. Best-effort: a network
		// failure here must not leave the user logged in locally; the backend
		// falls back to heartbeat-timeout detection instead.
		if err := phelixgrpc.NotifyLogout(connstate.Disconnected); err != nil {
			fmt.Printf("  %s Could not notify backend of logout: %v\n", color.YellowString("⚠"), err)
		}

		if err := performLogout(session); err != nil {
			return err
		}
		if err := removeSession(); err != nil {
			return err
		}
		connstate.MarkDisconnected()
		fmt.Println("✓ Successfully logged out.")
		fmt.Println("  Local apps and the monitor daemon are still running; dashboard sync is paused.")
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
