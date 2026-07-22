package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/spf13/cobra"
)

var (
	healthPath          string
	healthInterval      string
	healthRetries       int
	healthExpectedCodes string
	healthTimeout       string
	healthName          string
	healthURL           string
	watchFlag           bool
	healthMode          string
)

var HealthCmd = &cobra.Command{
	Use:   "health",
	Short: "Manage application health checks",
	Long:  "Configure and monitor health check endpoints for applications",
}

// health set <AppID|AppName> --path /health --interval 10s --retries 3
var healthSetCmd = &cobra.Command{
	Use:   "set <ID|AppName>",
	Short: "Initialize health checks for an application",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("failed to load app state: %w", err)
		}

		appID, err := resolveAppID(args[0])
		if err != nil {
			return err
		}

		appInfo := findApp(appID)
		if appInfo == nil {
			return fmt.Errorf("app not found: %s", args[0])
		}

		// Initialize config manager
		configMgr, err := health.InitConfigManager()
		if err != nil {
			return fmt.Errorf("failed to initialize config: %w", err)
		}

		// Get or create config
		config := configMgr.GetConfig(appID)
		if config == nil {
			config = &health.AppHealthConfig{
				AppID:     appID,
				AppName:   appInfo.Name,
				Endpoints: make(map[string]*health.HealthCheckConfig),
				Enabled:   true,
			}
		}

		// Parse interval
		interval := "10s"
		if healthInterval != "" {
			interval = healthInterval
			// Validate duration format
			if _, err := time.ParseDuration(interval); err != nil {
				return fmt.Errorf("invalid interval format: %s", interval)
			}
		}

		// Parse retries
		retries := 3
		if healthRetries > 0 {
			retries = healthRetries
		}

		timeout := "10s"
		if healthTimeout != "" {
			timeout = healthTimeout
			if _, err := time.ParseDuration(timeout); err != nil {
				return fmt.Errorf("invalid timeout format: %s", timeout)
			}
		}

		expectedCodes := "200-299"
		if healthExpectedCodes != "" {
			expectedCodes = healthExpectedCodes
		}

		// Create default endpoint if path is provided
		if healthPath != "" {
			endpointName := "default"
			url := fmt.Sprintf("http://localhost:%d%s", appInfo.Port, healthPath)

			config.Endpoints[endpointName] = &health.HealthCheckConfig{
				Name:          endpointName,
				URL:           url,
				Interval:      interval,
				Retries:       retries,
				ExpectedCodes: expectedCodes,
				Timeout:       timeout,
			}

			fmt.Printf("Default endpoint configured: %s\n", endpointName)
		} else {
			fmt.Println("Health checks initialized for app (no endpoints yet)")
		}

		// Persist the deploy-tier health config (used by blue-green/rolling
		// deploys). Validate the mode up front so typos are caught here rather
		// than silently falling back to auto at deploy time.
		mode := health.TierModeAuto
		if healthMode != "" {
			switch health.DeployTierMode(healthMode) {
			case health.TierModeAuto, health.TierModeHTTP, health.TierModeTCPOnly, health.TierModeNone:
				mode = health.DeployTierMode(healthMode)
			default:
				return fmt.Errorf("invalid --mode %q: must be one of auto, http, tcp-only, none", healthMode)
			}
		}
		config.DeployTier = &health.DeployTierConfig{
			Mode:     mode,
			Path:     healthPath,
			Interval: interval,
			Retries:  retries,
			Timeout:  timeout,
		}

		// Save config
		if err := configMgr.SaveConfig(appID, config); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		fmt.Printf("✓ Health checks configured for '%s' (ID: %s)\n", appInfo.Name, appID)
		fmt.Println("Use 'phelix health add' to add more endpoints")

		return nil
	},
}

// health add <AppID|AppName> --name "API health" --url https://api.example.com/health
var healthAddCmd = &cobra.Command{
	Use:   "add <ID|AppName>",
	Short: "Add a health check endpoint",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("failed to load app state: %w", err)
		}

		appID, err := resolveAppID(args[0])
		if err != nil {
			return err
		}

		appInfo := findApp(appID)
		if appInfo == nil {
			return fmt.Errorf("app not found: %s", args[0])
		}

		if healthName == "" {
			return fmt.Errorf("endpoint name is required (--name)")
		}

		if healthURL == "" {
			return fmt.Errorf("endpoint URL is required (--url)")
		}

		// Initialize config manager
		configMgr, err := health.InitConfigManager()
		if err != nil {
			return fmt.Errorf("failed to initialize config: %w", err)
		}

		// Get or create config
		config := configMgr.GetConfig(appID)
		if config == nil {
			config = &health.AppHealthConfig{
				AppID:     appID,
				AppName:   appInfo.Name,
				Endpoints: make(map[string]*health.HealthCheckConfig),
				Enabled:   true,
			}
		}

		// Check if endpoint already exists
		if _, exists := config.Endpoints[healthName]; exists {
			return fmt.Errorf("endpoint '%s' already exists", healthName)
		}

		// Parse parameters
		interval := "10s"
		if healthInterval != "" {
			interval = healthInterval
			if _, err := time.ParseDuration(interval); err != nil {
				return fmt.Errorf("invalid interval format: %s", interval)
			}
		}

		retries := 3
		if healthRetries > 0 {
			retries = healthRetries
		}

		timeout := "10s"
		if healthTimeout != "" {
			timeout = healthTimeout
			if _, err := time.ParseDuration(timeout); err != nil {
				return fmt.Errorf("invalid timeout format: %s", timeout)
			}
		}

		expectedCodes := "200-299"
		if healthExpectedCodes != "" {
			expectedCodes = healthExpectedCodes
		}

		// Create endpoint config
		config.Endpoints[healthName] = &health.HealthCheckConfig{
			Name:          healthName,
			URL:           healthURL,
			Interval:      interval,
			Retries:       retries,
			ExpectedCodes: expectedCodes,
			Timeout:       timeout,
		}

		// Save config
		if err := configMgr.SaveConfig(appID, config); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		fmt.Printf("✓ Endpoint '%s' added to '%s'\n", healthName, appInfo.Name)
		fmt.Printf("  URL: %s\n", healthURL)
		fmt.Printf("  Interval: %s, Retries: %d, Timeout: %s\n", interval, retries, timeout)

		return nil
	},
}

// health remove <AppID|AppName> --name "endpoint-name"
var healthRemoveCmd = &cobra.Command{
	Use:   "remove <ID|AppName>",
	Short: "Remove a health check endpoint",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("failed to load app state: %w", err)
		}

		appID, err := resolveAppID(args[0])
		if err != nil {
			return err
		}

		appInfo := findApp(appID)
		if appInfo == nil {
			return fmt.Errorf("app not found: %s", args[0])
		}

		if healthName == "" {
			return fmt.Errorf("endpoint name is required (--name)")
		}

		// Initialize config manager
		configMgr, err := health.InitConfigManager()
		if err != nil {
			return fmt.Errorf("failed to initialize config: %w", err)
		}

		config := configMgr.GetConfig(appID)
		if config == nil {
			return fmt.Errorf("no health checks configured for this app")
		}

		if _, exists := config.Endpoints[healthName]; !exists {
			return fmt.Errorf("endpoint '%s' not found", healthName)
		}

		delete(config.Endpoints, healthName)
		if err := configMgr.SaveConfig(appID, config); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		fmt.Printf("✓ Endpoint '%s' removed from '%s'\n", healthName, appInfo.Name)
		return nil
	},
}

// health list <AppID|AppName>
var healthListCmd = &cobra.Command{
	Use:   "list <ID|AppName>",
	Short: "List health check endpoints for an app",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("failed to load app state: %w", err)
		}

		appID, err := resolveAppID(args[0])
		if err != nil {
			return err
		}

		appInfo := findApp(appID)
		if appInfo == nil {
			return fmt.Errorf("app not found: %s", args[0])
		}

		// Initialize config manager
		configMgr, err := health.InitConfigManager()
		if err != nil {
			return fmt.Errorf("failed to initialize config: %w", err)
		}

		config := configMgr.GetConfig(appID)
		if config == nil {
			fmt.Printf("No health checks configured for '%s'\n", appInfo.Name)
			return nil
		}

		if len(config.Endpoints) == 0 {
			fmt.Printf("No endpoints configured for '%s'\n", appInfo.Name)
			return nil
		}

		fmt.Printf("Health Checks for '%s' (ID: %s)\n", appInfo.Name, appID)
		fmt.Println(strings.Repeat("=", 80))

		for endpointName, endpoint := range config.Endpoints {
			fmt.Printf("\nName: %s\n", endpointName)
			fmt.Printf("  URL: %s\n", endpoint.URL)
			fmt.Printf("  Interval: %s\n", endpoint.Interval)
			fmt.Printf("  Retries: %d\n", endpoint.Retries)
			fmt.Printf("  Timeout: %s\n", endpoint.Timeout)
			fmt.Printf("  Expected Status Codes: %s\n", endpoint.ExpectedCodes)
		}

		fmt.Println()
		return nil
	},
}

// health status <AppID|AppName>
var healthStatusCmd = &cobra.Command{
	Use:   "status <ID|AppName>",
	Short: "Show current health status for an app",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("failed to load app state: %w", err)
		}

		appID, err := resolveAppID(args[0])
		if err != nil {
			return err
		}

		appInfo := findApp(appID)
		if appInfo == nil {
			return fmt.Errorf("app not found: %s", args[0])
		}

		// Get the global daemon
		globalDaemon := health.GetGlobalDaemon()
		daemon, err := globalDaemon.GetDaemon()
		if err != nil {
			return fmt.Errorf("health daemon not ready: %w", err)
		}

		if watchFlag {
			// Live watch mode
			display := health.NewTerminalDisplay(appID, appInfo.Name, daemon)
			// Register display to receive updates from daemon
			daemon.AddEventListener(func(event interface{}) {
				display.NotifyUpdate()
			})
			display.Start()

			// Wait for interrupt signal
			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

			<-sigChan

			// Cleanup
			display.Stop()
			fmt.Println()
			return nil
		} else {
			// One-time status display
			health.PrintStatusTable(appID, appInfo.Name, daemon)
			return nil
		}
	},
}

// health watch <AppID|AppName> - live terminal display
// Deprecated: Use 'phelix health status <ID|AppName> --watch' instead
var healthWatchCmd = &cobra.Command{
	Use:   "watch <ID|AppName>",
	Short: "Watch health checks in real-time (deprecated, use 'status --watch')",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("failed to load app state: %w", err)
		}

		appID, err := resolveAppID(args[0])
		if err != nil {
			return err
		}

		appInfo := findApp(appID)
		if appInfo == nil {
			return fmt.Errorf("app not found: %s", args[0])
		}

		// Get the global daemon
		globalDaemon := health.GetGlobalDaemon()
		daemon, err := globalDaemon.GetDaemon()
		if err != nil {
			return fmt.Errorf("health daemon not ready: %w", err)
		}

		// Create terminal display
		display := health.NewTerminalDisplay(appID, appInfo.Name, daemon)
		// Register display to receive updates from daemon
		daemon.AddEventListener(func(event interface{}) {
			display.NotifyUpdate()
		})
		display.Start()

		// Wait for interrupt signal
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

		<-sigChan

		// Cleanup
		display.Stop()
		fmt.Println("\nWatching stopped")
		return nil
	},
}

// health daemon - start background daemon
var healthDaemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Health check daemon (runs automatically, this command is deprecated)",
	RunE: func(cmd *cobra.Command, args []string) error {
		globalDaemon := health.GetGlobalDaemon()
		if !globalDaemon.IsRunning() {
			fmt.Println("⚠️  The health check daemon is not running. It should start automatically.")
			fmt.Println("Please restart phelix or check the logs for startup errors.")
			return fmt.Errorf("daemon not running")
		}
		fmt.Println("✓ Health check daemon is running in the background")
		fmt.Println("  Use 'phelix health status <app>' to check health status")
		fmt.Println("  Use 'phelix health status <app> --watch' for live updates")
		return nil
	},
}

// Helper to resolve app ID or name to actual ID
func resolveAppID(idOrName string) (string, error) {
	// Try to parse as numeric ID first
	if _, err := strconv.Atoi(idOrName); err == nil {
		return idOrName, nil
	}

	// Try to find by name
	apps := app.Manager.ListApplications()
	for _, a := range apps {
		if a.Name == idOrName {
			return a.ID, nil
		}
	}

	// Return as-is (might be ID)
	return idOrName, nil
}

// Helper to find app by ID
func findApp(appID string) *app.AppListItem {
	apps := app.Manager.ListApplications()
	for _, a := range apps {
		if a.ID == appID {
			return &a
		}
	}
	return nil
}

func init() {
	// Add subcommands
	HealthCmd.AddCommand(healthSetCmd)
	HealthCmd.AddCommand(healthAddCmd)
	HealthCmd.AddCommand(healthRemoveCmd)
	HealthCmd.AddCommand(healthListCmd)
	HealthCmd.AddCommand(healthStatusCmd)
	HealthCmd.AddCommand(healthWatchCmd)
	HealthCmd.AddCommand(healthDaemonCmd)

	// health set flags
	healthSetCmd.Flags().StringVar(&healthPath, "path", "", "Health check path (e.g., /health)")
	healthSetCmd.Flags().StringVar(&healthInterval, "interval", "10s", "Check interval (e.g., 10s, 1m)")
	healthSetCmd.Flags().IntVar(&healthRetries, "retries", 3, "Number of consecutive failures before marking DOWN")
	healthSetCmd.Flags().StringVar(&healthExpectedCodes, "codes", "200-299", "Expected HTTP status codes (e.g., 200-299)")
	healthSetCmd.Flags().StringVar(&healthTimeout, "timeout", "10s", "Request timeout")
	healthSetCmd.Flags().StringVar(&healthMode, "mode", "auto", "Deploy health tier: auto, http, tcp-only, none")

	// health add flags
	healthAddCmd.Flags().StringVar(&healthName, "name", "", "Endpoint name (required)")
	healthAddCmd.Flags().StringVar(&healthURL, "url", "", "Endpoint URL (required)")
	healthAddCmd.Flags().StringVar(&healthInterval, "interval", "10s", "Check interval")
	healthAddCmd.Flags().IntVar(&healthRetries, "retries", 3, "Consecutive failures threshold")
	healthAddCmd.Flags().StringVar(&healthExpectedCodes, "codes", "200-299", "Expected HTTP status codes")
	healthAddCmd.Flags().StringVar(&healthTimeout, "timeout", "10s", "Request timeout")
	healthAddCmd.MarkFlagRequired("name")
	healthAddCmd.MarkFlagRequired("url")

	// health remove flags
	healthRemoveCmd.Flags().StringVar(&healthName, "name", "", "Endpoint name (required)")
	healthRemoveCmd.MarkFlagRequired("name")

	// health status flags
	healthStatusCmd.Flags().BoolVar(&watchFlag, "watch", false, "Watch health checks in real-time")
}
