package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
)

var ListCmd = &cobra.Command{
	Use:   "list",
	Short: "Lists all applications with their ID, status, PID, and uptime",
	RunE: func(cmd *cobra.Command, args []string) error {
		apps := app.Manager.ListApplications()
		if len(apps) == 0 {
			fmt.Println("No applications found.")
			return nil
		}

		proxyUp, proxyByApp := loadProxySnapshot()

		table := createTable()
		populateTable(table, apps, proxyByApp)
		table.Render()

		printProxySummary(proxyUp)
		return nil
	},
}

func createTable() *tablewriter.Table {
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"ID", "Name", "Status", "Language", "PID", "Uptime", "Deploy", "Proxy"})
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

func populateTable(table *tablewriter.Table, apps []app.AppListItem, proxyByApp map[string]proxy.AppStatus) {
	for _, a := range apps {
		status := FormatStatus(a.Status)
		lang := a.Language
		if lang == "" {
			lang = "unknown"
		}
		deployCol, proxyCol := formatDeployProxyColumns(a.Name, proxyByApp)
		table.Append([]string{
			a.ID,
			color.BlueString(a.Name),
			status,
			color.CyanString(lang),
			fmt.Sprintf("%d", a.PID),
			a.Uptime,
			deployCol,
			proxyCol,
		})
	}
}

// formatDeployProxyColumns returns human-readable Deploy and Proxy cells for
// an app, based on deploy.json (if present) and the live proxy daemon state.
func formatDeployProxyColumns(appName string, proxyByApp map[string]proxy.AppStatus) (deployCol, proxyCol string) {
	deployCol = color.HiBlackString("-")
	proxyCol = color.HiBlackString("off")

	if state, err := deploy.Load(appName); err == nil && state != nil {
		switch state.Mode {
		case deploy.ModeBlueGreen:
			if state.ActiveSlot != "" {
				deployCol = color.MagentaString("blue-green (%s)", state.ActiveSlot)
			} else {
				deployCol = color.MagentaString("blue-green")
			}
		case deploy.ModeRolling:
			n := len(state.Replicas)
			deployCol = color.MagentaString("rolling (×%d)", n)
		default:
			if state.Mode != "" {
				deployCol = color.MagentaString(string(state.Mode))
			}
		}
	}

	if ps, ok := proxyByApp[appName]; ok {
		proxyCol = color.GreenString("on :%d → %s", ps.PublicPort, ps.Primary.Label)
	}
	return deployCol, proxyCol
}

// loadProxySnapshot returns whether the daemon is up and a map of enrolled
// apps. Failures are silent — list/status still work without the proxy.
func loadProxySnapshot() (bool, map[string]proxy.AppStatus) {
	out := make(map[string]proxy.AppStatus)
	socket, err := proxy.DefaultSocketPath()
	if err != nil {
		return false, out
	}
	client := proxy.NewClient(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		return false, out
	}
	statuses, err := client.Status(ctx, "")
	if err != nil {
		return true, out
	}
	for _, s := range statuses {
		out[s.AppName] = s
	}
	return true, out
}

func printProxySummary(proxyUp bool) {
	fmt.Println()
	if proxyUp {
		fmt.Printf("%s Proxy daemon: %s  (%s)\n",
			color.GreenString("✓"),
			color.GreenString("running"),
			color.YellowString("phelix proxy status"))
	} else {
		fmt.Printf("%s Proxy daemon: %s  (start with %s)\n",
			color.YellowString("⚠"),
			color.HiBlackString("not running"),
			color.CyanString("phelix proxy"))
	}
}
