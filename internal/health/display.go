package health

import (
	"fmt"
	"os"
	"time"

	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
)

// TerminalDisplay manages the live-updating terminal UI
type TerminalDisplay struct {
	appID      string
	appName    string
	daemon     *Daemon
	stopChan   chan struct{}
	updateChan chan struct{}
	isRunning  bool
	lastDrawn  map[string]*HealthCheckResult
}

// NewTerminalDisplay creates a new terminal display
func NewTerminalDisplay(appID, appName string, daemon *Daemon) *TerminalDisplay {
	return &TerminalDisplay{
		appID:      appID,
		appName:    appName,
		daemon:     daemon,
		stopChan:   make(chan struct{}),
		updateChan: make(chan struct{}, 100),
		lastDrawn:  make(map[string]*HealthCheckResult),
	}
}

// Start begins the display loop
func (td *TerminalDisplay) Start() {
	if td.isRunning {
		return
	}
	td.isRunning = true

	// Clear screen
	td.clearScreen()

	// Initial draw
	td.draw()

	// Draw on updates
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-td.stopChan:
				return
			case <-td.updateChan:
				td.draw()
			case <-ticker.C:
				// Periodically redraw in case something changed
				td.draw()
			}
		}
	}()
}

// Stop halts the display
func (td *TerminalDisplay) Stop() {
	if !td.isRunning {
		return
	}
	td.isRunning = false
	close(td.stopChan)
}

// NotifyUpdate signals that a health check was updated
func (td *TerminalDisplay) NotifyUpdate() {
	if td.isRunning {
		select {
		case td.updateChan <- struct{}{}:
		default:
			// Channel full, skip
		}
	}
}

// draw renders the terminal UI
func (td *TerminalDisplay) draw() {
	td.clearScreen()

	// Header
	headerColor := color.New(color.FgCyan, color.Bold)
	headerColor.Printf("Health Checks — %s\n\n", td.appName)

	// Get current status
	status := td.daemon.GetStatus(td.appID)
	if len(status) == 0 {
		fmt.Println("No health checks configured.")
		return
	}

	// Get config for URL display
	config := td.daemon.configManager.GetConfig(td.appID)
	if config == nil {
		fmt.Println("Configuration not found.")
		return
	}

	// Create table
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"NAME", "URL", "LATENCY", "CODE", "STATUS", "LAST CHECK"})
	table.SetBorder(true)
	table.SetRowLine(false)
	table.SetAlignment(tablewriter.ALIGN_LEFT)

	// Add rows
	for endpointName, result := range status {
		endpointConfig := config.Endpoints[endpointName]
		if endpointConfig == nil {
			continue
		}

		row := []string{
			endpointName,
			td.truncateURL(endpointConfig.URL),
			td.formatLatency(result.LatencyMs),
			td.formatStatusCode(result.StatusCode),
			td.formatStatus(result.Status),
			td.formatCheckTime(result.CheckedAt),
		}

		// Color the row based on status
		table.Rich(row, []tablewriter.Colors{
			{},
			{},
			{},
			{},
			td.getStatusColors(result.Status),
			{},
		})
	}

	table.Render()

	// Legend
	fmt.Println()
	color.Green("UP - Health check passed")
	color.Red("DOWN - Health check failed or unexpected status code")
	color.Yellow("TIMEOUT - Health check timed out")

	// Auto-restart info
	fmt.Println()
	fmt.Println("Press Ctrl+C to stop watching")
}

// clearScreen clears the terminal screen
func (td *TerminalDisplay) clearScreen() {
	fmt.Print("\033[2J\033[H")
}

// truncateURL truncates long URLs for display
func (td *TerminalDisplay) truncateURL(url string) string {
	if len(url) > 50 {
		return url[:47] + "..."
	}
	return url
}

// formatLatency formats latency for display
func (td *TerminalDisplay) formatLatency(latency *int64) string {
	if latency == nil {
		return "—"
	}
	return fmt.Sprintf("%dms", *latency)
}

// formatStatusCode formats HTTP status code for display
func (td *TerminalDisplay) formatStatusCode(code *int) string {
	if code == nil {
		return "—"
	}
	return fmt.Sprintf("%d", *code)
}

// formatStatus formats status with appropriate coloring
func (td *TerminalDisplay) formatStatus(status string) string {
	return status
}

// getStatusColors returns table writer colors based on status
func (td *TerminalDisplay) getStatusColors(status string) tablewriter.Colors {
	switch status {
	case "UP":
		return tablewriter.Colors{tablewriter.FgGreenColor}
	case "DOWN":
		return tablewriter.Colors{tablewriter.FgRedColor}
	case "TIMEOUT":
		return tablewriter.Colors{tablewriter.FgYellowColor}
	default:
		return tablewriter.Colors{}
	}
}

// formatCheckTime formats the last check time
func (td *TerminalDisplay) formatCheckTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}

	duration := time.Since(t)
	if duration < time.Minute {
		return "just now"
	} else if duration < time.Hour {
		minutes := int(duration.Minutes())
		if minutes == 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", minutes)
	} else if duration < 24*time.Hour {
		hours := int(duration.Hours())
		if hours == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", hours)
	}

	return duration.String() + " ago"
}

// PrintStatusTable prints a one-time status table (for commands)
func PrintStatusTable(appID, appName string, daemon *Daemon) {
	config := daemon.configManager.GetConfig(appID)
	if config == nil {
		fmt.Println("No health checks configured for this app.")
		return
	}

	status := daemon.GetStatus(appID)

	// Create table
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"NAME", "URL", "LATENCY", "CODE", "STATUS", "LAST CHECK"})
	table.SetBorder(true)
	table.SetRowLine(false)
	table.SetAlignment(tablewriter.ALIGN_LEFT)

	for endpointName, endpointConfig := range config.Endpoints {
		result := status[endpointName]
		if result == nil {
			result = &HealthCheckResult{
				EndpointName: endpointName,
				Status:       "UNKNOWN",
			}
		}

		row := []string{
			endpointName,
			truncateURLDisplay(endpointConfig.URL),
			formatLatencyDisplay(result.LatencyMs),
			formatStatusCodeDisplay(result.StatusCode),
			result.Status,
			formatCheckTimeDisplay(result.CheckedAt),
		}

		table.Rich(row, []tablewriter.Colors{
			{},
			{},
			{},
			{},
			getStatusColorsDisplay(result.Status),
			{},
		})
	}

	table.Render()
}

// Helper functions for static printing
func truncateURLDisplay(url string) string {
	if len(url) > 50 {
		return url[:47] + "..."
	}
	return url
}

func formatLatencyDisplay(latency *int64) string {
	if latency == nil {
		return "—"
	}
	return fmt.Sprintf("%dms", *latency)
}

func formatStatusCodeDisplay(code *int) string {
	if code == nil {
		return "—"
	}
	return fmt.Sprintf("%d", *code)
}

func getStatusColorsDisplay(status string) tablewriter.Colors {
	switch status {
	case "UP":
		return tablewriter.Colors{tablewriter.FgGreenColor}
	case "DOWN":
		return tablewriter.Colors{tablewriter.FgRedColor}
	case "TIMEOUT":
		return tablewriter.Colors{tablewriter.FgYellowColor}
	default:
		return tablewriter.Colors{}
	}
}

func formatCheckTimeDisplay(t time.Time) string {
	if t.IsZero() {
		return "never"
	}

	duration := time.Since(t)
	if duration < time.Minute {
		return "just now"
	} else if duration < time.Hour {
		minutes := int(duration.Minutes())
		if minutes == 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", minutes)
	} else if duration < 24*time.Hour {
		hours := int(duration.Hours())
		if hours == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", hours)
	}

	return duration.String() + " ago"
}
