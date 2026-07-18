package cmd

import (
	"fmt"
	"strings"

	"github.com/abdorrahmani/phelix/internal/env"
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
			return fmt.Errorf("application not found: %v", err)
		}

		appID := appInfo.ID
		appName := appInfo.Name
		if appName == "" {
			appName = appID
		}

		switch subcommand {
		case "set":
			if len(args) < 3 {
				return fmt.Errorf("usage: phelix env set <AppName> <KEY=VALUE> [KEY=VALUE ...]")
			}

			// Process all KEY=VALUE pairs
			for i := 2; i < len(args); i++ {
				kvPair := args[i]
				parts := strings.SplitN(kvPair, "=", 2)
				if len(parts) != 2 {
					return fmt.Errorf("invalid format: %s (expected KEY=VALUE)", kvPair)
				}

				key := strings.TrimSpace(parts[0])
				value := parts[1]

				if err := env.Instance.SetEnv(appID, key, value); err != nil {
					return fmt.Errorf("failed to set env var '%s': %v", key, err)
				}

				fmt.Printf("✓ Set '%s' for application '%s'\n", key, appName)
			}

		case "get":
			if len(args) < 3 {
				return fmt.Errorf("usage: phelix env get <AppName> <KEY>")
			}

			key := args[2]
			value, err := env.Instance.GetEnv(appID, key)
			if err != nil {
				return fmt.Errorf("failed to get env var: %v", err)
			}

			// Mask if sensitive
			displayValue := env.MaskValue(key, value)
			fmt.Printf("%s=%s\n", key, displayValue)

		case "list":
			vars, err := env.Instance.ListEnv(appID)
			if err != nil {
				return fmt.Errorf("failed to list env vars: %v", err)
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
				return fmt.Errorf("usage: phelix env unset <AppName> <KEY>")
			}

			key := args[2]
			if err := env.Instance.UnsetEnv(appID, key); err != nil {
				return fmt.Errorf("failed to unset env var: %v", err)
			}

			fmt.Printf("✓ Unset '%s' for application '%s'\n", key, appName)

		case "check":
			if len(args) < 3 {
				return fmt.Errorf("usage: phelix env check <AppName> <KEY>")
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
			return fmt.Errorf("unknown subcommand: %s\nUse: set, get, list, unset, check", subcommand)
		}

		return nil
	},
}
