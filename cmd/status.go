package cmd

import (
	"fmt"
	"os"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"github.com/spf13/cobra"
)

var StatusCmd = &cobra.Command{
	Use:   "status <ID|AppName>",
	Short: "Displays the status of a specific application by its ID or AppName",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		identifier := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
		}

		status, err := app.Manager.StatusApplication(identifier)
		if err != nil {
			// Return as-is: StatusApplication already produces a precise code
			// (NOT_FOUND for a missing app), and re-wrapping with PROCESS_FAILED
			// would mask it and turn exit 12 into a generic exit 1.
			return err
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
	table := tablewriter.NewTable(os.Stdout,
		tablewriter.WithRendition(tw.Rendition{
			Settings: tw.Settings{Separators: tw.Separators{BetweenRows: tw.On}},
		}),
		tablewriter.WithConfig(tablewriter.Config{
			Header: tw.CellConfig{
				Alignment: tw.CellAlignment{Global: tw.AlignCenter},
			},
			Row: tw.CellConfig{
				Alignment: tw.CellAlignment{
					Global:    tw.AlignCenter,
					PerColumn: []tw.Align{tw.AlignLeft},
				},
			},
		}),
	)
	table.Header([]string{"ID", "Name", "Language", "Status", "PID", "Uptime", "RAM Usage (MB)", "CPU Usage (%)"})
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

	// --- Version info (from versions.json) --------------------------------
	fmt.Printf("%s Version\n", color.BlueString("→"))
	meta, err := deploy.CurrentVersionMeta(appName)
	if err != nil || meta == nil {
		fmt.Printf("  Current version: %s\n", color.HiBlackString("— (no version history)"))
	} else {
		verLabel := fmt.Sprintf("v%d", meta.Version)
		if meta.Tag != "" {
			verLabel += fmt.Sprintf(" %s", color.YellowString("(%s)", meta.Tag))
		}
		fmt.Printf("  Current version: %s\n", color.CyanString(verLabel))
		if meta.GitCommit != "" {
			commit := meta.GitCommit
			if len(commit) > 12 {
				commit = commit[:12]
			}
			fmt.Printf("  Git commit:      %s\n", color.HiBlackString(commit))
		}
		fmt.Printf("  Built at:        %s\n", meta.BuiltAt.Format("2006-01-02 15:04:05"))
		if meta.DeployedAt != nil {
			fmt.Printf("  Deployed at:     %s\n", meta.DeployedAt.Format("2006-01-02 15:04:05"))
		}
		if meta.SizeBytes > 0 {
			fmt.Printf("  Binary size:     %.1f MB\n", float64(meta.SizeBytes)/(1024*1024))
		}
	}

	// --- Recent version history (last 3) ----------------------------------
	recent, recentErr := deploy.RecentVersions(appName, 3)
	if recentErr == nil && len(recent) > 0 {
		fmt.Printf("  Recent versions: ")
		for i, v := range recent {
			if i > 0 {
				fmt.Printf(", ")
			}
			label := fmt.Sprintf("v%d", v.Version)
			if v.Tag != "" {
				label += fmt.Sprintf(" (%s)", v.Tag)
			}
			if v.IsCurrent {
				label += " *"
			}
			fmt.Printf("%s", label)
		}
		fmt.Println()
	}

	// --- Deploy state (blue-green / rolling) ------------------------------
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
			if state.ActiveVersion > 0 {
				fmt.Printf("  Active version: v%d\n", state.ActiveVersion)
			}
			fmt.Printf("  Public port:   %d\n", state.PublicPort)
			if state.Slots != nil {
				for _, slot := range []string{deploy.SlotBlue, deploy.SlotGreen} {
					if inst := state.Slots[slot]; inst != nil {
						ver := ""
						if inst.Version > 0 {
							ver = fmt.Sprintf(" version=v%d", inst.Version)
						}
						fmt.Printf("  Slot %-5s:    status=%s pid=%d port=%d%s\n",
							slot, inst.Status, inst.PID, inst.Port, ver)
					}
				}
			}
		case deploy.ModeRolling:
			fmt.Printf("  Deploy method: %s\n", color.MagentaString("rolling"))
			if state.ActiveVersion > 0 {
				fmt.Printf("  Active version: v%d\n", state.ActiveVersion)
			}
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
		if state.LastRollback != nil {
			fmt.Printf("  Last rollback: v%d → v%d at %s\n",
				state.LastRollback.FromVersion, state.LastRollback.ToVersion,
				state.LastRollback.At.Format("2006-01-02 15:04:05"))
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
