package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/autoscale"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var autoscaleJSON bool

// AutoscaleCmd groups the autoscaling management surface: runtime status,
// the config-level enable/disable switch, the runtime pause/resume intent,
// one-shot evaluation and the scaling history.
var AutoscaleCmd = &cobra.Command{
	Use:   "autoscale",
	Short: "Inspect and manage automatic replica scaling",
	Args:  cobra.NoArgs,
}

func init() {
	AutoscaleCmd.PersistentFlags().BoolVar(&autoscaleJSON, "json", false, "Emit machine-readable JSON")
	AutoscaleCmd.AddCommand(autoscaleStatusCmd, autoscaleEnableCmd, autoscaleDisableCmd,
		autoscalePauseCmd, autoscaleResumeCmd, autoscaleEvaluateCmd, autoscaleHistoryCmd)
}

func autoscaleResolveApp(args []string) (*app.AppInfo, error) {
	if len(args) == 0 {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "missing required <ID|AppName>")
	}
	if err := app.Manager.LoadState(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
	}
	return GetAppInfo(args[0])
}

// autoscaleProxyClient resolves the proxy control client, or nil when the
// proxy socket is unavailable (resource-only evaluation).
func autoscaleProxyClient() *proxy.Client {
	socket, err := proxy.DefaultSocketPath()
	if err != nil {
		return nil
	}
	return proxy.NewClient(socket)
}

var autoscaleStatusCmd = &cobra.Command{
	Use:   "status [ID|AppName]",
	Short: "Show an app's effective autoscaling configuration and runtime state",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if autoscaleJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		appInfo, err := autoscaleResolveApp(args)
		if err != nil {
			return err
		}

		view, err := buildAutoscaleStatus(appInfo)
		if err != nil {
			return err
		}
		if autoscaleJSON {
			return writeEnvelopeResult(machine.Success("", view))
		}
		renderAutoscaleStatus(view)
		return nil
	},
}

// autoscaleStatusView is the status command's machine contract.
type autoscaleStatusView struct {
	App                 string                     `json:"app"`
	AppID               string                     `json:"app_id"`
	Mode                string                     `json:"mode"`
	Replicas            int                        `json:"replicas"`
	DesiredReplicas     int                        `json:"desired_replicas"`
	Enabled             bool                       `json:"enabled"`
	Paused              bool                       `json:"paused"`
	PausedAt            string                     `json:"paused_at,omitempty"`
	PausedBy            string                     `json:"paused_by,omitempty"`
	LastEvaluation      string                     `json:"last_evaluation,omitempty"`
	LastSuccessfulScale string                     `json:"last_successful_scale,omitempty"`
	LastDecision        *autoscale.HistoryRecord   `json:"last_decision,omitempty"`
	ReplicaMetrics      []autoscale.ReplicaSample  `json:"replica_metrics,omitempty"`
	Settings            *project.AutoscaleSettings `json:"settings,omitempty"`
	RecentUser          []autoscale.HistoryRecord  `json:"recent_history,omitempty"`
}

func buildAutoscaleStatus(appInfo *app.AppInfo) (*autoscaleStatusView, error) {
	view := &autoscaleStatusView{App: appInfo.Name, AppID: appInfo.ID}

	var state *deploy.DeployState
	if loaded, err := deploy.Load(appInfo.Name); err == nil {
		state = loaded
		view.Mode = string(state.Mode)
		view.Replicas = len(state.Replicas)
		view.DesiredReplicas = len(state.Replicas)
	}

	pause, err := autoscale.LoadPauseState(appInfo.Name)
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to read autoscale runtime state", err)
	}
	view.Paused = pause.Paused
	if pause.Paused {
		if !pause.PausedAt.IsZero() {
			view.PausedAt = pause.PausedAt.UTC().Format(time.RFC3339)
		}
		view.PausedBy = pause.PausedBy
	}

	if cfg, cfgErr := project.Load(appInfo.Directory); cfgErr == nil && cfg != nil && cfg.Deploy != nil && cfg.Deploy.Autoscaling != nil {
		s := cfg.Deploy.Autoscaling.Resolve(cfg.Deploy.Replicas)
		view.Enabled = s.Enabled
		view.Settings = &s
	}

	records, err := autoscale.ReadHistory(appInfo.Name, 50)
	if err == nil && len(records) > 0 {
		view.RecentUser = records
		if len(view.RecentUser) > 5 {
			view.RecentUser = view.RecentUser[len(view.RecentUser)-5:]
		}
		last := records[len(records)-1]
		view.LastDecision = &last
		view.LastEvaluation = last.Time
		for i := len(records) - 1; i >= 0; i-- {
			if records[i].Kind == autoscale.HistoryExecuted {
				view.LastSuccessfulScale = records[i].Time
				break
			}
		}
	}
	if runtime := autoscale.RuntimeStateFor(appInfo.Name); !runtime.LastEvaluation.IsZero() {
		view.LastEvaluation = runtime.LastEvaluation.UTC().Format(time.RFC3339)
		if runtime.LastDecision != nil {
			view.LastDecision = runtime.LastDecision
		}
		if !runtime.LastSuccessfulScale.IsZero() {
			view.LastSuccessfulScale = runtime.LastSuccessfulScale.UTC().Format(time.RFC3339)
		}
	}
	if view.LastDecision != nil && view.LastDecision.To > 0 {
		view.DesiredReplicas = view.LastDecision.To
	}
	if refs := autoscale.ResourceRefs(state); len(refs) > 0 {
		sampler := autoscale.NewSampler(appInfo.Name, nil)
		sampler.SetMemoryLimit(autoscale.MemoryLimitBytesForApp(appInfo.Name))
		view.ReplicaMetrics = sampler.SampleResources(refs, statusSampleInterval)
	}
	return view, nil
}

func renderAutoscaleStatus(v *autoscaleStatusView) {
	state := "disabled"
	if v.Enabled {
		state = "enabled"
	}
	if v.Paused {
		state += " (paused"
		if v.PausedBy != "" {
			state += " by " + v.PausedBy
		}
		state += ")"
	}
	fmt.Printf("Autoscaling for %s: %s\n", color.CyanString("'%s'", v.App), state)
	if v.Mode != "" {
		fmt.Printf("  Mode: %s, replicas: %d", v.Mode, v.Replicas)
		if v.DesiredReplicas > 0 && v.DesiredReplicas != v.Replicas {
			fmt.Printf(" (desired: %d)", v.DesiredReplicas)
		}
		fmt.Println()
	}
	if v.LastEvaluation != "" {
		fmt.Printf("  Last evaluation: %s\n", v.LastEvaluation)
	}
	if v.LastSuccessfulScale != "" {
		fmt.Printf("  Last successful scale: %s\n", v.LastSuccessfulScale)
	}
	if d := v.LastDecision; d != nil {
		line := fmt.Sprintf("  Last decision: %s %s", d.Kind, d.Direction)
		if d.From != d.To {
			line += fmt.Sprintf(" %d->%d", d.From, d.To)
		}
		if d.Reason != "" {
			line += " (" + d.Reason + ")"
		}
		fmt.Println(line)
	}
	if len(v.ReplicaMetrics) > 0 {
		fmt.Println("  Replica resources:")
		for _, sample := range v.ReplicaMetrics {
			fmt.Printf("    %-10s%s\n", sample.Key, formatReplicaResources(sample))
		}
	}
	if s := v.Settings; s != nil {
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintf(w, "  Interval:\t%s\n", s.Interval)
		fmt.Fprintf(w, "  Min/max replicas:\t%d / %d\n", s.MinReplicas, s.MaxReplicas)
		fmt.Fprintf(w, "  CPU up/down:\t%.0f%% / %.0f%%\n", s.ScaleUpCPU, s.ScaleDownCPU)
		if s.ScaleUpMemory > 0 || s.ScaleDownMemory > 0 {
			fmt.Fprintf(w, "  Memory up/down:\t%.0f%% / %.0f%%\n", s.ScaleUpMemory, s.ScaleDownMemory)
		}
		if s.ScaleUpRPS > 0 || s.ScaleDownRPS > 0 {
			fmt.Fprintf(w, "  RPS up/down:\t%.0f / %.0f\n", s.ScaleUpRPS, s.ScaleDownRPS)
		}
		fmt.Fprintf(w, "  P95 up/down:\t%s / %s\n", s.ScaleUpP95, s.ScaleDownP95)
		if s.ScaleUpP99 > 0 || s.ScaleDownP99 > 0 {
			fmt.Fprintf(w, "  P99 up/down:\t%s / %s\n", s.ScaleUpP99, s.ScaleDownP99)
		}
		fmt.Fprintf(w, "  Windows up/down:\t%d / %d\n", s.ScaleUpWindows, s.ScaleDownWindows)
		fmt.Fprintf(w, "  Cooldown up/down:\t%s / %s\n", s.ScaleUpCooldown, s.ScaleDownCooldown)
		if s.StabilizationWindow > 0 {
			fmt.Fprintf(w, "  Stabilization window:\t%s\n", s.StabilizationWindow)
		}
		w.Flush()
	}
	if len(v.RecentUser) > 0 {
		fmt.Println("  Recent scaling activity:")
		for _, rec := range v.RecentUser {
			line := fmt.Sprintf("    %s  %-10s %s", rec.Time, rec.Kind, rec.Direction)
			if rec.From != rec.To {
				line += fmt.Sprintf(" %d->%d", rec.From, rec.To)
			}
			if rec.Error != "" {
				line += "  " + rec.Error
			}
			fmt.Println(line)
		}
	}
}

var autoscaleEnableCmd = &cobra.Command{
	Use:   "enable [ID|AppName]",
	Short: "Enable automatic scaling in the app's phelix.yaml",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAutoscaleToggle(args, true)
	},
}

var autoscaleDisableCmd = &cobra.Command{
	Use:   "disable [ID|AppName]",
	Short: "Disable automatic scaling in the app's phelix.yaml",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAutoscaleToggle(args, false)
	},
}

func runAutoscaleToggle(args []string, enabled bool) error {
	if autoscaleJSON {
		restore := machine.EnterJSON()
		defer restore()
	}
	appInfo, err := autoscaleResolveApp(args)
	if err != nil {
		return err
	}
	if err := project.SetAutoscalingEnabled(appInfo.Directory, enabled); err != nil {
		return err
	}
	if autoscaleJSON {
		return writeEnvelopeResult(machine.Success("", map[string]any{"app": appInfo.Name, "enabled": enabled}))
	}
	word := "disabled"
	if enabled {
		word = "enabled"
	}
	fmt.Printf("%s Autoscaling %s for %s (the monitor controller picks this up on its next evaluation)\n",
		color.GreenString("✓"), word, color.CyanString("'%s'", appInfo.Name))
	return nil
}

var autoscalePauseCmd = &cobra.Command{
	Use:   "pause [ID|AppName] --reason TEXT",
	Short: "Pause automatic scaling for an app at runtime (survives restarts)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAutoscalePause(args, true)
	},
}

var autoscaleResumeCmd = &cobra.Command{
	Use:   "resume [ID|AppName]",
	Short: "Resume automatic scaling for a paused app",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAutoscalePause(args, false)
	},
}

var autoscaleReason string

func runAutoscalePause(args []string, pause bool) error {
	if autoscaleJSON {
		restore := machine.EnterJSON()
		defer restore()
	}
	appInfo, err := autoscaleResolveApp(args)
	if err != nil {
		return err
	}
	var pauseErr error
	if pause {
		pauseErr = autoscale.Pause(appInfo.Name, "operator", autoscaleReason)
	} else {
		pauseErr = autoscale.Resume(appInfo.Name)
	}
	if pauseErr != nil {
		return pauseErr
	}
	if autoscaleJSON {
		return writeEnvelopeResult(machine.Success("", map[string]any{"app": appInfo.Name, "paused": pause}))
	}
	word := "resumed"
	if pause {
		word = "paused"
	}
	fmt.Printf("%s Autoscaling %s for %s\n", color.GreenString("✓"), word, color.CyanString("'%s'", appInfo.Name))
	return nil
}

var autoscaleEvaluateCmd = &cobra.Command{
	Use:   "evaluate [ID|AppName]",
	Short: "Run one autoscaling evaluation now and report the decision",
	Long: `Runs one synchronous evaluation: sample metrics, apply the decision engine,
and execute any resulting scaling through the app's normal scaling path.
Engine windows and cooldowns are per-process: this one-shot evaluation does
not share streak state with a running monitor controller, but the app deploy
lock and the executor's stale-decision rejection keep both safe.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if autoscaleJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		appInfo, err := autoscaleResolveApp(args)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		decision, execErr := autoscale.EvaluateOnce(ctx, autoscale.AppRef{
			Name: appInfo.Name, ID: appInfo.ID, Directory: appInfo.Directory,
		}, autoscaleProxyClient())
		if autoscaleJSON {
			return writeEnvelopeResult(machine.Success("", map[string]any{
				"app": appInfo.Name, "direction": decision.Direction,
				"current": decision.CurrentReplicas, "desired": decision.DesiredReplicas,
				"reason": decision.Reason, "error": errString(execErr),
			}))
		}
		switch decision.Direction {
		case deploy.ScaleUp:
			fmt.Printf("%s Scale up: %d -> %d (%s)\n", color.GreenString("↑"), decision.CurrentReplicas, decision.DesiredReplicas, decision.Reason)
		case deploy.ScaleDown:
			fmt.Printf("%s Scale down: %d -> %d (%s)\n", color.GreenString("↓"), decision.CurrentReplicas, decision.DesiredReplicas, decision.Reason)
		default:
			fmt.Printf("%s No scaling: %s\n", color.BlueString("·"), decision.Reason)
		}
		if execErr != nil {
			return execErr
		}
		return nil
	},
}

var autoscaleHistoryCmd = &cobra.Command{
	Use:   "history [ID|AppName]",
	Short: "Show the app's scaling decisions and outcomes",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if autoscaleJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		appInfo, err := autoscaleResolveApp(args)
		if err != nil {
			return err
		}
		records, err := autoscale.ReadHistory(appInfo.Name, autoscaleHistoryLimit)
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to read autoscale history", err)
		}
		if autoscaleJSON {
			return writeEnvelopeResult(machine.Success("", records))
		}
		if len(records) == 0 {
			fmt.Printf("No scaling history for %s\n", color.CyanString("'%s'", appInfo.Name))
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "TIME\tKIND\tDIRECTION\tCHANGE\tDETAIL")
		for _, rec := range records {
			change := fmt.Sprintf("%d->%d", rec.From, rec.To)
			if rec.From == rec.To {
				change = "-"
			}
			detail := rec.Reason
			if rec.Error != "" {
				detail = rec.Error
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", rec.Time, rec.Kind, rec.Direction, change, detail)
		}
		w.Flush()
		return nil
	},
}

var autoscaleHistoryLimit int

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func init() {
	autoscalePauseCmd.Flags().StringVar(&autoscaleReason, "reason", "", "Why autoscaling is paused (recorded with the pause)")
	autoscaleHistoryCmd.Flags().IntVar(&autoscaleHistoryLimit, "limit", 20, "Most recent records to show (0 for all)")
}
