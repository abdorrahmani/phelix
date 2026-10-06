package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"github.com/spf13/cobra"
)

var listJSON bool

var ListCmd = &cobra.Command{
	Use:   "list",
	Short: "Lists all applications with their ID, status, PID, and uptime",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Machine mode: table + proxy summary detour to stderr so the JSON
		// envelope stays alone on stdout.
		if listJSON {
			restore := machine.EnterJSON()
			defer restore()
		}

		apps := app.Manager.ListApplications()

		// Reconcile zero-downtime deployments first so the top-level STATUS
		// column reflects deployment reality (active slot process alive or
		// not), then re-read the list.
		for _, a := range apps {
			reconcileAppWithDeploy(a.Name)
		}
		apps = app.Manager.ListApplications()

		if len(apps) == 0 {
			fmt.Println("No applications found.")
			return writeEnvelopeResult(machine.Success("", listResult{Apps: []appView{}}))
		}

		proxyUp, proxyByApp := loadProxySnapshot()

		table := createTable()
		populateTable(table, apps, proxyByApp)
		table.Render()

		printProxySummary(proxyUp)
		return writeEnvelopeResult(machine.Success("", buildListResult(apps, proxyUp, proxyByApp)))
	},
}

func init() {
	ListCmd.Flags().BoolVar(&listJSON, "json", false,
		"Output machine-readable JSON (stdout carries only the result envelope; table moves to stderr)")
}

// appView is the machine-contract view of one managed application.
type appView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Language      string `json:"language,omitempty"`
	Status        string `json:"status"`
	Watching      bool   `json:"watching"`
	PID           int    `json:"pid,omitempty"`
	Uptime        string `json:"uptime,omitempty"`
	Version       int    `json:"version,omitempty"`
	Tag           string `json:"tag,omitempty"`
	DeployMode    string `json:"deploy_mode,omitempty"`
	ActiveSlot    string `json:"active_slot,omitempty"`
	Replicas      int    `json:"replicas,omitempty"`
	ProxyEnrolled bool   `json:"proxy_enrolled"`
	ProxyPort     int    `json:"proxy_port,omitempty"`
}

type listResult struct {
	Apps      []appView `json:"apps"`
	ProxyUp   bool      `json:"proxy_up"`
	ProxyApps int       `json:"proxy_enrolled_apps"`
}

// buildListResult projects the app list + proxy snapshot onto the machine
// contract. Status is the reconciled lifecycle value (running/stopped/
// degraded) — the raw app record, not the colorized table cell.
func buildListResult(apps []app.AppListItem, proxyUp bool, proxyByApp map[string]proxy.AppStatus) *listResult {
	views := make([]appView, 0, len(apps))
	for _, a := range apps {
		v := appView{
			ID:       a.ID,
			Name:     a.Name,
			Language: a.Language,
			Status:   a.Status,
			Watching: a.Watching,
			PID:      a.PID,
			Uptime:   a.Uptime,
		}
		if meta, err := deploy.CurrentVersionMeta(a.Name); err == nil && meta != nil {
			v.Version = meta.Version
			v.Tag = meta.Tag
		}
		if state, err := deploy.Load(a.Name); err == nil && state != nil {
			v.DeployMode = string(state.Mode)
			v.ActiveSlot = state.ActiveSlot
			v.Replicas = len(state.Replicas)
		}
		if ps, ok := proxyByApp[a.Name]; ok {
			v.ProxyEnrolled = true
			v.ProxyPort = ps.PublicPort
		}
		views = append(views, v)
	}
	return &listResult{Apps: views, ProxyUp: proxyUp, ProxyApps: len(proxyByApp)}
}

func createTable() *tablewriter.Table {
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
	table.Header([]string{"ID", "Name", "Version", "Status", "Watching", "Language", "PID", "Uptime", "Deploy", "Proxy"})
	return table
}

func populateTable(table *tablewriter.Table, apps []app.AppListItem, proxyByApp map[string]proxy.AppStatus) {
	for _, a := range apps {
		status := FormatStatus(a.Status)
		lang := a.Language
		if lang == "" {
			lang = "unknown"
		}
		verCol := formatVersionColumn(a.Name)
		deployCol, proxyCol := formatDeployProxyColumns(a.Name, proxyByApp)
		table.Append([]string{
			a.ID,
			color.BlueString(a.Name),
			verCol,
			status,
			FormatWatching(a.Watching),
			color.CyanString(lang),
			fmt.Sprintf("%d", a.PID),
			a.Uptime,
			deployCol,
			proxyCol,
		})
	}
}

// formatVersionColumn returns a human-readable version cell for an app,
// joining AppInfo + DeployState + versions.json data. Returns "—" when the
// app has no version history (e.g. predating this feature or never built
// through the versioned path).
func formatVersionColumn(appName string) string {
	meta, err := deploy.CurrentVersionMeta(appName)
	if err != nil || meta == nil {
		return color.HiBlackString("—")
	}
	label := fmt.Sprintf("v%d", meta.Version)
	if meta.Tag != "" {
		label += " " + color.YellowString("(%s)", meta.Tag)
	}
	return color.CyanString(label)
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
