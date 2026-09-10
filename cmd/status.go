package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"github.com/spf13/cobra"
)

var StatusCmd = &cobra.Command{
	Use:   "status [ID|AppName]",
	Short: "Displays the status of a specific application by its ID or AppName",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "missing required <ID|AppName>; usage: phelix status <ID|AppName>")
			}
			identifier, err := PromptApp(false, "Select application to inspect")
			if err != nil {
				return err
			}
			args = []string{identifier}
		}

		identifier := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
		}

		// Reconcile zero-downtime deployment reality into the lifecycle record
		// before rendering: the app is "running" only when the active slot's
		// process is alive, never merely because deploy.json says so.
		reconcileAppWithDeploy(identifier)

		status, err := app.Manager.StatusApplication(identifier)
		if err != nil {
			// Return as-is: StatusApplication already produces a precise code
			// (NOT_FOUND for a missing app), and re-wrapping with PROCESS_FAILED
			// would mask it and turn exit 12 into a generic exit 1.
			return err
		}

		displayStatus(status)
		displayHTTPMetrics(status.ID)
		displayDeployAndProxy(status.Name)
		return nil
	},
}

// displayHTTPMetrics samples an optionally configured Caddy endpoint. Two
// samples establish one refresh interval without retrying failures.
func displayHTTPMetrics(appID string) {
	configMgr, err := health.InitConfigManager()
	if err != nil {
		logs.WarningFile("status", "failed initialize HTTP metrics config: %v", err)
		return
	}

	config := configMgr.GetConfig(appID)
	if config == nil || config.HTTPMetrics == nil || !config.HTTPMetrics.Configured() {
		return
	}

	var source health.HTTPMetricsSource = health.NewCaddyMetricsSource()
	ctx, cancel := context.WithTimeout(context.Background(), 3*health.CaddyMetricsInterval)
	defer cancel()

	snapshot, err := source.Scrape(ctx, *config.HTTPMetrics)
	if err != nil {
		logs.WarningFile("status", "failed collect HTTP metrics: %v", err)
		return
	}
	if !snapshot.HasRequestDelta {
		select {
		case <-ctx.Done():
		case <-time.After(health.CaddyMetricsInterval):
		}
		snapshot, err = source.Scrape(ctx, *config.HTTPMetrics)
		if err != nil {
			logs.WarningFile("status", "failed refresh HTTP metrics: %v", err)
		}
	}
	if snapshot == nil {
		return
	}

	requestRate := "-"
	if snapshot.HasRequestDelta {
		requestRate = fmt.Sprintf("%.1f", snapshot.RequestsPerSecond)
	}
	clientRate := "-"
	serverRate := "-"
	if snapshot.HasRequestDelta {
		clientRate = fmt.Sprintf("%.1f", snapshot.ClientErrorsPerSec)
		serverRate = fmt.Sprintf("%.1f", snapshot.ServerErrorsPerSec)
	}

	fmt.Println()
	fmt.Printf("%s HTTP (via Caddy)\n", color.BlueString("→"))
	fmt.Printf("  Requests/sec:     %s\n", requestRate)
	fmt.Printf("  Latency (est.):   p50 %s p95 %s p99 %s\n",
		formatMetricDuration(snapshot.LatencyP50),
		formatMetricDuration(snapshot.LatencyP95),
		formatMetricDuration(snapshot.LatencyP99))
	fmt.Printf("  Errors:           4xx: %s/s 5xx: %s/s\n", clientRate, serverRate)
}

func formatMetricDuration(duration time.Duration) string {
	if duration <= 0 {
		return "-"
	}
	return duration.String()
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

// sortedReplicaKeysForDisplay orders replica indices numerically so status
// output lists replica-0..N deterministically regardless of JSON map order.
func sortedReplicaKeysForDisplay(m map[string]*deploy.Instance) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, aerr := strconv.Atoi(keys[i])
		b, berr := strconv.Atoi(keys[j])
		if aerr == nil && berr == nil {
			return a < b
		}
		return keys[i] < keys[j]
	})
	return keys
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
			for _, key := range sortedReplicaKeysForDisplay(state.Replicas) {
				inst := state.Replicas[key]
				if inst == nil {
					continue
				}
				ver := ""
				if inst.Version > 0 {
					ver = fmt.Sprintf(" version=v%d", inst.Version)
				}
				fmt.Printf("  Replica %-3s:   status=%s pid=%d port=%d%s\n",
					key, inst.Status, inst.PID, inst.Port, ver)
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
		// Case C: instance alive but proxy down — the process runs internally,
		// yet nothing serves it publicly. Distinguish the two.
		if state != nil {
			switch inst := state.ServingInstance(); {
			case inst != nil && deploy.InstanceAlive(inst):
				fmt.Printf("  %s Proxy is down but the %s instance (pid %d) is still running — it is not publicly reachable.\n",
					color.YellowString("⚠"), inst.Slot, inst.PID)
			case state.ActiveSlot != "" && state.ActiveInstance() != nil && state.ActiveInstance().Status == "running":
				fmt.Printf("  %s Proxy is down and the active slot's recorded process is dead — run %s to restore it.\n",
					color.YellowString("⚠"), color.CyanString("phelix start "+appName))
			}
		}
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
		// Invariant: for blue-green, the slot the deployment calls active MUST
		// be the one the proxy routes to. Anything else means traffic and
		// deploy.json disagree — surface it instead of rendering two "green"
		// lines that contradict each other.
		if state != nil && state.Mode == deploy.ModeBlueGreen && state.ActiveSlot != "" &&
			ps.Primary.Label != "" && ps.Primary.Label != state.ActiveSlot {
			fmt.Printf("  %s Proxy routes to %q but deploy state says slot %q is active — run %s to re-align, or redeploy.\n",
				color.RedString("✗"), ps.Primary.Label, state.ActiveSlot,
				color.CyanString("phelix start "+appName))
		}
		// Case D: proxy points at a slot whose recorded instance is dead.
		if state != nil && state.Mode == deploy.ModeBlueGreen && state.ActiveSlot != "" {
			if inst := state.ActiveInstance(); inst != nil && !deploy.InstanceAlive(inst) && inst.PID > 0 {
				fmt.Printf("  %s Proxy routes to slot %s whose process (pid %d) is dead — traffic is failing. Run %s or redeploy.\n",
					color.RedString("✗"), inst.Slot, inst.PID, color.CyanString("phelix start "+appName))
			}
		}
	} else {
		fmt.Printf("  Proxy:         %s (daemon up, app not enrolled)\n",
			color.YellowString("not enrolled"))
	}
}
