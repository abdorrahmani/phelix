package cmd

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	grpcClient "github.com/abdorrahmani/phelix/internal/grpc"
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
	daemonForeground    bool
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

		// Sync to backend via gRPC
		grpcClient.SendHealthSetConfig(appID, appInfo.Name, healthPath, interval, timeout, expectedCodes, string(mode), retries)

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

		// Save config locally
		if err := configMgr.SaveConfig(appID, config); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		// Sync to backend via gRPC
		grpcClient.SendHealthAddEndpoint(appID, appInfo.Name, config.Endpoints[healthName])

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

		// Sync to backend via gRPC
		grpcClient.SendHealthRemoveEndpoint(appID, appInfo.Name, healthName)

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

		// Try the global daemon first
		globalDaemon := health.GetGlobalDaemon()
		daemon, err := globalDaemon.GetDaemon()
		if err != nil {
			// Daemon not running — do a one-shot check
			return runOneShotHealthCheck(appID, appInfo)
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
			fmt.Println("Health daemon is not running. For continuous monitoring, run:")
			fmt.Println("  phelix monitor")
			fmt.Println()
			fmt.Println("For a one-time check, run:")
			fmt.Printf("  phelix health status %s\n", args[0])
			return nil
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

// health daemon - start health check daemon (background by default)
var healthDaemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Start the health check daemon (background by default)",
	Long: `Start the health check daemon that monitors application health endpoints.

By default the daemon starts in the background and the CLI exits. Use
--foreground to run attached to the terminal. Use 'phelix health daemon status'
to inspect the daemon, and 'phelix health daemon stop' to shut it down.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if daemonForeground {
			return runHealthDaemonForeground()
		}
		return startDaemonBackground()
	},
}

var healthDaemonStatusCmd = &cobra.Command{
	Use:           "status",
	Short:         "Show health daemon status",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		pid, err := readHealthDaemonPID()
		if err != nil {
			fmt.Printf("⚠ Health daemon is not running\n")
			fmt.Printf("  Start it with: phelix health daemon\n")
			return nil
		}

		// Check if process is actually alive
		proc, err := os.FindProcess(pid)
		if err != nil || proc.Signal(syscall.Signal(0)) != nil {
			// Stale PID file
			_ = removeHealthDaemonPIDFile()
			fmt.Printf("⚠ Health daemon is not running (stale PID file cleaned up)\n")
			fmt.Printf("  Start it with: phelix health daemon\n")
			return nil
		}

		fmt.Printf("✓ Health daemon is running (PID %d)\n", pid)
		fmt.Printf("  PID file: %s\n", healthDaemonPIDPath())
		fmt.Printf("  Status: phelix health status <app>\n")
		fmt.Printf("  Stop:   phelix health daemon stop\n")
		return nil
	},
}

var healthDaemonStopCmd = &cobra.Command{
	Use:           "stop",
	Short:         "Stop the background health daemon",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		pid, err := readHealthDaemonPID()
		if err != nil {
			fmt.Printf("⚠ Health daemon is not running\n")
			return nil
		}

		proc, err := os.FindProcess(pid)
		if err != nil {
			_ = removeHealthDaemonPIDFile()
			fmt.Printf("⚠ Health daemon is not running (stale PID file cleaned up)\n")
			return nil
		}

		if err := proc.Signal(syscall.SIGTERM); err != nil {
			_ = removeHealthDaemonPIDFile()
			fmt.Printf("⚠ Health daemon is not running (stale PID file cleaned up)\n")
			return nil
		}

		// Wait up to 5s for graceful shutdown
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if proc.Signal(syscall.Signal(0)) != nil {
				_ = removeHealthDaemonPIDFile()
				fmt.Printf("✓ Health daemon stopped\n")
				return nil
			}
			time.Sleep(200 * time.Millisecond)
		}

		// Force kill if still alive
		_ = proc.Kill()
		_ = removeHealthDaemonPIDFile()
		fmt.Printf("✓ Health daemon stopped (force killed)\n")
		return nil
	},
}

// startDaemonBackground forks a detached `phelix health daemon --foreground`
// child and returns so the CLI can exit.
func startDaemonBackground() error {
	if pid, err := readHealthDaemonPID(); err == nil {
		// PID file exists — check if process is alive
		if proc, findErr := os.FindProcess(pid); findErr == nil && proc.Signal(syscall.Signal(0)) == nil {
			fmt.Printf("✓ Health daemon is already running (PID %d)\n", pid)
			fmt.Printf("  Status: phelix health status <app>\n")
			fmt.Printf("  Stop:   phelix health daemon stop\n")
			return nil
		}
		// Stale PID file
		_ = removeHealthDaemonPIDFile()
	}

	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	c := exec.Command(exe, "health", "daemon", "--foreground")
	c.SysProcAttr = health.DetachedSysProcAttr()
	if devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		c.Stdin = devnull
		c.Stdout = devnull
		c.Stderr = devnull
	}
	if err := c.Start(); err != nil {
		return fmt.Errorf("✗ failed to start health daemon: %v", err)
	}
	_ = c.Process.Release()

	// Wait briefly for PID file to appear
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid, err := readHealthDaemonPID(); err == nil {
			if proc, findErr := os.FindProcess(pid); findErr == nil && proc.Signal(syscall.Signal(0)) == nil {
				fmt.Printf("✓ Health daemon started in background (PID %d)\n", pid)
				fmt.Printf("  Status: phelix health status <app>\n")
				fmt.Printf("  Stop:   phelix health daemon stop\n")
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("✗ health daemon did not start within 5s")
}

// runHealthDaemonForeground is the long-running daemon loop executed by the
// detached child (and users who pass --foreground).
func runHealthDaemonForeground() error {
	globalDaemon := health.GetGlobalDaemon()

	if globalDaemon.IsRunning() {
		fmt.Println("Health check daemon is already running")
		return nil
	}

	// Write PID file
	if err := writeHealthDaemonPID(); err != nil {
		return fmt.Errorf("failed to write PID file: %w", err)
	}
	defer removeHealthDaemonPIDFile()

	// Set up signal handler to clean up PID file on exit
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("Starting health check daemon...")

	// Initialize gRPC client for backend reporting
	grpcClient.InitGlobalClient()
	c := grpcClient.GetClient()
	go c.Start()

	// Wait for connection to establish
	time.Sleep(2 * time.Second)

	if c.IsConnected() {
		reporter := grpcClient.NewGrpcHealthReporter(c.GetServiceClient())
		globalDaemon.SetReporter(reporter)
		fmt.Println("  Backend reporting: gRPC")
	} else {
		fmt.Println("  Backend reporting: disabled (not connected)")
	}

	if err := globalDaemon.Start(); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	fmt.Println("Health check daemon started")
	fmt.Println("  Use 'phelix health status <app>' to check health status")
	fmt.Println("  Use 'phelix health status <app> --watch' for live updates")

	// Keep running until interrupted
	<-sigChan

	fmt.Println("\nStopping daemon...")
	if err := globalDaemon.Stop(); err != nil {
		log.Printf("[Health] Error stopping daemon: %v", err)
	}
	fmt.Println("Daemon stopped")
	return nil
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

// runOneShotHealthCheck performs a single health check round and prints results.
// Used when the daemon is not running (standalone CLI usage).
func runOneShotHealthCheck(appID string, appInfo *app.AppListItem) error {
	configMgr, err := health.InitConfigManager()
	if err != nil {
		return fmt.Errorf("failed to initialize config: %w", err)
	}

	config := configMgr.GetConfig(appID)
	if config == nil || len(config.Endpoints) == 0 {
		fmt.Printf("No health checks configured for '%s'\n", appInfo.Name)
		return nil
	}

	checker := health.NewChecker()

	fmt.Printf("Health Checks for '%s' (ID: %s)\n", appInfo.Name, appID)
	fmt.Println(strings.Repeat("=", 80))

	for name, ep := range config.Endpoints {
		result := checker.Check(ep)
		statusIcon := "✓"
		if result.Status != "UP" {
			statusIcon = "✗"
		}

		fmt.Printf("\n%s Name: %s\n", statusIcon, name)
		fmt.Printf("  URL: %s\n", result.URL)
		fmt.Printf("  Status: %s\n", result.Status)
		if result.StatusCode != nil {
			fmt.Printf("  HTTP Status: %d\n", *result.StatusCode)
		}
		if result.LatencyMs != nil {
			fmt.Printf("  Latency: %dms\n", *result.LatencyMs)
		}
		if result.Error != nil {
			fmt.Printf("  Error: %s\n", *result.Error)
		}
	}

	fmt.Println()
	return nil
}

// healthDaemonPIDPath returns ~/.phelix/health_daemon.pid
func healthDaemonPIDPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".phelix", "health_daemon.pid")
}

// writeHealthDaemonPID writes the current process PID to the PID file.
func writeHealthDaemonPID() error {
	return os.WriteFile(healthDaemonPIDPath(), []byte(strconv.Itoa(os.Getpid())), 0644)
}

// readHealthDaemonPID reads the PID from the PID file.
func readHealthDaemonPID() (int, error) {
	data, err := os.ReadFile(healthDaemonPIDPath())
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// removeHealthDaemonPIDFile removes the PID file.
func removeHealthDaemonPIDFile() error {
	return os.Remove(healthDaemonPIDPath())
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
	healthDaemonCmd.AddCommand(healthDaemonStatusCmd)
	healthDaemonCmd.AddCommand(healthDaemonStopCmd)

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

	// health daemon flags
	healthDaemonCmd.Flags().BoolVar(&daemonForeground, "foreground", false, "Run the daemon in the foreground instead of detaching")
}
