package cmd

import (
	"fmt"
	"strings"

	"github.com/abdorrahmani/phelix/internal/env"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/spf13/cobra"
)

var EnvCmd = &cobra.Command{
	Use:   "env <set|get|list|unset|check> <AppName> [KEY[=VALUE]]",
	Short: "Manage encrypted environment variables for applications",
	Long: `Manage encrypted environment variables for applications.
	
Examples:
  phelix env set MyApp DATABASE_URL=postgresql://localhost/db
  phelix env get MyApp DATABASE_URL
  phelix env list MyApp
  phelix env unset MyApp DATABASE_URL
  phelix env check MyApp DATABASE_URL`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		subcommand := args[0]
		appIdentifier := args[1]

		// Get app info to validate it exists
		appInfo, err := GetAppInfo(appIdentifier)
		if err != nil {
			return err
		}

		appID := appInfo.ID
		appName := appInfo.Name
		if appName == "" {
			appName = appID
		}

		switch subcommand {
		case "set":
			if len(args) < 3 {
				return phelixerr.Newf(
					phelixerr.CodeInvalidArgument,
					"usage: phelix env set <AppName> <KEY=VALUE> [KEY=VALUE ...]",
				)
			}

			// Process all KEY=VALUE pairs
			for i := 2; i < len(args); i++ {
				kvPair := args[i]
				parts := strings.SplitN(kvPair, "=", 2)
				if len(parts) != 2 {
					return phelixerr.Newf(
						phelixerr.CodeInvalidArgument,
						"invalid format: %s (expected KEY=VALUE)",
						kvPair,
					)
				}

				key := strings.TrimSpace(parts[0])
				value := parts[1]

				if err := env.Instance.SetEnv(appID, key, value); err != nil {
					return phelixerr.Wrapf(
						phelixerr.CodeEncryption,
						err,
						"failed to set env var %q",
						key,
					)
				}

				fmt.Printf("✓ Set '%s' for application '%s'\n", key, appName)
				phelixgrpc.ReportEvent(appID, appName, "env_change", true, "", 0, "", "")
			}

		case "get":
			if len(args) < 3 {
				return phelixerr.Newf(
					phelixerr.CodeInvalidArgument,
					"usage: phelix env get <AppName> <KEY>",
				)
			}

			key := args[2]
			value, err := env.Instance.GetEnv(appID, key)
			if err != nil {
				return phelixerr.Wrapf(
					phelixerr.CodeEncryption,
					err,
					"failed to get env var %q",
					key,
				)
			}

			// Mask if sensitive
			displayValue := env.MaskValue(key, value)
			fmt.Printf("%s=%s\n", key, displayValue)

		case "list":
			vars, err := env.Instance.ListEnv(appID)
			if err != nil {
				return phelixerr.Wrap(phelixerr.CodeEncryption, "failed to list env vars", err)
			}

			if len(vars) == 0 {
				fmt.Printf("No environment variables set for application '%s'\n", appName)
				return nil
			}

			fmt.Printf("Environment variables for '%s':\n", appName)
			for key := range vars {
				fmt.Printf("  • %s=%s\n", key, "***REDACTED***")
			}

		case "unset":
			if len(args) < 3 {
				return phelixerr.Newf(
					phelixerr.CodeInvalidArgument,
					"usage: phelix env unset <AppName> <KEY>",
				)
			}

			key := args[2]
			if err := env.Instance.UnsetEnv(appID, key); err != nil {
				return phelixerr.Wrapf(
					phelixerr.CodeEncryption,
					err,
					"failed to unset env var %q",
					key,
				)
			}

			fmt.Printf("✓ Unset '%s' for application '%s'\n", key, appName)
			phelixgrpc.ReportEvent(appID, appName, "env_change", true, "", 0, "", "")

		case "check":
			if len(args) < 3 {
				return phelixerr.Newf(
					phelixerr.CodeInvalidArgument,
					"usage: phelix env check <AppName> <KEY>",
				)
			}

			key := args[2]
			_, err := env.Instance.GetEnv(appID, key)
			if err != nil {
				// Variable not set
				fmt.Printf("✗ Environment variable '%s' is not set for application '%s'\n", key, appName)
				return nil
			}

			fmt.Printf("✓ Environment variable '%s' is set for application '%s'\n", key, appName)

		default:
			return phelixerr.Newf(
				phelixerr.CodeInvalidArgument,
				"unknown subcommand: %s\nUse: set, get, list, unset, check",
				subcommand,
			)
		}

		return nil
	},
}
