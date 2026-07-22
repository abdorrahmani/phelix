package cmd

import (
	"fmt"
	"os"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
)

var StatusCmd = &cobra.Command{
	Use:   "status <ID|AppName>",
	Short: "Displays the status of a specific application by its ID or AppName",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		identifier := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("failed to load state: %v", err)
		}

		status, err := app.Manager.StatusApplication(identifier)
		if err != nil {
			return fmt.Errorf("error getting status: %v", err)
		}

		displayStatus(status)
		displayDeployAndProxy(status.Name)
		return nil
	},
}

func displayStatus(status app.AppStatus) {
	table := createStatusTable()
	populateStatusTable(table, status)
	table.Render()
}

func createStatusTable() *tablewriter.Table {
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"ID", "Name", "Language", "Status", "PID", "Uptime", "RAM Usage (MB)", "CPU Usage (%)"})
	table.SetBorder(true)
	table.SetRowLine(true)
	table.SetColumnAlignment([]int{
		tablewriter.ALIGN_LEFT,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
	})
	return table
}

func populateStatusTable(table *tablewriter.Table, status app.AppStatus) {
	statusText := FormatStatus(status.Status)
	ramUsageMB := float64(status.RAMUsage) / (1024 * 1024)
	lang := status.Language
	if lang == "" {
		lang = "unknown"
	}

	table.Append([]string{
		status.ID,
		color.BlueString(status.Name),
		color.CyanString(lang),
		statusText,
		fmt.Sprintf("%d", status.PID),
		status.Uptime,
		fmt.Sprintf("%.2f", ramUsageMB),
		fmt.Sprintf("%.2f", status.CPUUsage),
	})
}

// displayDeployAndProxy prints zero-downtime deploy mode and live proxy
// routing for the app, when available.
func displayDeployAndProxy(appName string) {
	proxyUp, proxyByApp := loadProxySnapshot()

	fmt.Println()
	fmt.Printf("%s Zero-downtime deploy\n", color.BlueString("→"))

	state, err := deploy.Load(appName)
	if err != nil || state == nil {
		fmt.Printf("  Deploy method: %s\n", color.HiBlackString("none (classic stop→start)"))
	} else {
		switch state.Mode {
		case deploy.ModeBlueGreen:
			active := state.ActiveSlot
			if active == "" {
				active = "—"
			}
			fmt.Printf("  Deploy method: %s\n", color.MagentaString("blue-green"))
			fmt.Printf("  Active slot:   %s\n", color.CyanString(active))
			fmt.Printf("  Public port:   %d\n", state.PublicPort)
			if state.Slots != nil {
				for _, slot := range []string{deploy.SlotBlue, deploy.SlotGreen} {
					if inst := state.Slots[slot]; inst != nil {
						fmt.Printf("  Slot %-5s:    status=%s pid=%d port=%d\n",
							slot, inst.Status, inst.PID, inst.Port)
					}
				}
			}
		case deploy.ModeRolling:
			fmt.Printf("  Deploy method: %s\n", color.MagentaString("rolling"))
			fmt.Printf("  Replicas:      %d\n", len(state.Replicas))
			fmt.Printf("  Public port:   %d\n", state.PublicPort)
			for key, inst := range state.Replicas {
				if inst == nil {
					continue
				}
				fmt.Printf("  Replica %-3s:   status=%s pid=%d port=%d\n",
					key, inst.Status, inst.PID, inst.Port)
			}
		default:
			fmt.Printf("  Deploy method: %s\n", color.MagentaString(string(state.Mode)))
		}
		if state.Health != nil && state.Health.TierLabel != "" {
			fmt.Printf("  Health tier:   %s\n", state.Health.TierLabel)
		}
	}

	fmt.Println()
	if !proxyUp {
		fmt.Printf("  Proxy:         %s  (start with %s)\n",
			color.HiBlackString("not running"),
			color.CyanString("phelix proxy"))
		return
	}
	if ps, ok := proxyByApp[appName]; ok {
		fmt.Printf("  Proxy:         %s\n", color.GreenString("enrolled"))
		fmt.Printf("  Public port:   :%d\n", ps.PublicPort)
		fmt.Printf("  Primary:       %s (%s)\n", color.CyanString(ps.Primary.Label), ps.Primary.Host)
		if len(ps.Backends) > 0 {
			fmt.Printf("  Backends:      ")
			for i, b := range ps.Backends {
				if i > 0 {
					fmt.Printf(", ")
				}
				fmt.Printf("%s(%s)", b.Label, b.Host)
			}
			fmt.Println()
		}
		fmt.Printf("  In-flight:     %d\n", ps.InFlight)
	} else {
		fmt.Printf("  Proxy:         %s (daemon up, app not enrolled)\n",
			color.YellowString("not enrolled"))
	}
}
