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
	username   string
	apiKey     string
	loginScope string
)

// LoginCmd handles the "phelix auth" login command.
var LoginCmd = &cobra.Command{
	Use:   "login --username <username> --apikey <apikey> [--scope agent]",
	Short: "Authenticate user with Phelix",
	Long: `Authenticate user with Phelix.

By default the login issues a full-scope session (dashboard + monitoring),
stored in ~/.phelix/session.json — this is what interactive CLI commands use.

Use --scope agent to issue a monitoring-only session for the phelix monitor
daemon, stored separately in ~/.phelix/agent-session.json. An agent-scoped
token can only be used by the monitoring channel: if the server it runs on is
ever compromised, the stolen token cannot touch your dashboard or account.
Use this variant when provisioning servers (install scripts, systemd setup).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if loginScope != "" && loginScope != ScopeAgent && loginScope != ScopeFull {
			return phelixerr.Newf(
				phelixerr.CodeInvalidArgument,
				"invalid --scope %q: must be %q or %q",
				loginScope, ScopeAgent, ScopeFull,
			)
		}
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

		if err := authenticate(username, apiKey, loginScope); err != nil {
			// A rate-limited login already carries its operator-facing
			// message and code (RATE_LIMITED with the Retry-After window);
			// re-wrapping it as INVALID_CREDENTIALS would mangle both.
			if phelixerr.CodeOf(err) == phelixerr.CodeRateLimited {
				return err
			}
			return phelixerr.Wrapf(
				phelixerr.CodeInvalidCredentials,
				err,
				"authentication failed for username %q",
				username,
			)
		}

		if loginScope == ScopeAgent {
			// An agent-scoped token is rejected by the dashboard's REST
			// endpoints, so the post-login app upload is skipped by design.
			// The gRPC monitor surface (which the token IS valid for) syncs
			// apps through its own metadata/snapshot flow.
			fmt.Printf("Hi %s. Monitoring-only (agent-scoped) session created for the phelix monitor daemon.\n", color.New(color.Bold).SprintFunc()(username))
			fmt.Printf("A compromised server can no longer expose your full account: this token only authorizes monitoring.\n")
			connstate.MarkConnected()
			return nil
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
	LoginCmd.Flags().StringVar(&loginScope, "scope", "", "Token scope: 'agent' issues a monitoring-only session for the daemon (recommended when provisioning servers)")
}

// StatusCmd prints session status.
var StatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Shows authentication status",
	RunE: func(cmd *cobra.Command, args []string) error {
		session, err := GetValidSession()
		if err != nil {
			// No interactive session, but the daemon may still have its own
			// agent-scoped session — report that instead of a bare error.
			if agentSession, ok := GetValidAgentSession(); ok {
				printAgentSession(agentSession, true)
				printConnectionState()
				return nil
			}
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
		if agentSession, ok := GetValidAgentSession(); ok {
			printAgentSession(agentSession, false)
		}
		printConnectionState()
		return nil
	},
}

// scopeLabel renders a session scope for status output; empty (pre-scope
// sessions) is rendered as the full scope it actually is.
func scopeLabel(scope string) string {
	if scope == "" {
		return ScopeFull
	}
	return scope
}

// printAgentSession reports the daemon's separate agent-scoped session.
// only=true is used when no interactive session exists (daemon-only
// provisioning via `phelix auth login --scope agent`).
func printAgentSession(session *Session, only bool) {
	green := color.New(color.FgGreen).SprintFunc()
	fmt.Printf("• Monitor daemon session: %s (scope: %s, expires %s)\n",
		green(session.SessionID), scopeLabel(session.Scope),
		session.ExpiresAt.Format(time.RFC1123))
	if only {
		fmt.Printf("• No interactive session — dashboard/REST commands need 'phelix auth login'\n")
	}
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
		loggedOutAgent := false

		// The daemon's agent-scoped session, when present, is its own server
		// session record; log it out alongside the interactive one.
		if agentSession, ok := GetValidAgentSession(); ok {
			if err := performLogout(agentSession); err != nil {
				fmt.Printf("  %s Could not log out the agent-scoped session: %v\n", color.YellowString("⚠"), err)
			} else if err := removeAgentSession(); err != nil {
				return err
			} else {
				loggedOutAgent = true
			}
		}

		session, err := GetValidSession()
		if err != nil {
			if !loggedOutAgent {
				fmt.Println("No active session found.")
				return nil
			}
			// Agent-only provisioning: the daemon session above was the
			// only one, and it is gone now.
			connstate.MarkDisconnected()
			fmt.Println("✓ Successfully logged out (agent-scoped session).")
			fmt.Println("  Local apps and the monitor daemon are still running; dashboard sync is paused.")
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
	fmt.Printf("• Session scope: %s\n", green(scopeLabel(session.Scope)))
	if status != nil {
		fmt.Printf("• Session: %s\n", status.SessionID)
		fmt.Printf("• CLI: %s (%s/%s)\n", status.CliVersion, status.OS, status.Arch)
	}
	fmt.Printf("• Expires at: %s\n", session.ExpiresAt.Format(time.RFC1123))
}
