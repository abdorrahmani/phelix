package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/authz"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var scaleReplicas int
var scaleJSON bool
var scaleRequestKey string

var ScaleCmd = &cobra.Command{
	Use:           "scale [ID|AppName] --replicas N",
	Short:         "Change a worker or rolling service replica count in place, without rebuilding.",
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) (err error) {
		if scaleJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		if scaleReplicas < 1 {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"missing required --replicas N (the desired replica count, >= 1)")
		}
		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "missing required <ID|AppName>; usage: phelix scale <ID|AppName> --replicas N")
			}
			identifier, err := PromptApp(false, "Select app to scale")
			if err != nil {
				return err
			}
			args = []string{identifier}
		}

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
		}
		appInfo, err := GetAppInfo(args[0])
		if err != nil {
			return err
		}
		state, err := deploy.Load(appInfo.Name)
		if err != nil {
			return err
		}
		if state.Mode != deploy.ModeWorker && state.Mode != deploy.ModeRolling {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"%s is a %s deployment; phelix scale changes replica counts for type: worker apps and rolling services", appInfo.Name, state.Mode)
		}
		if err := validateScaleRange(appInfo.Directory, scaleReplicas); err != nil {
			return err
		}

		if authzErr := authorizeDirectExecution(authz.ActionScale, appInfo.Name, appInfo.ID); authzErr != nil {
			return authzErr
		}

		op := newOpRun(ops.KindScale, appInfo.Name, scaleRequestKey)
		defer func() { op.finish(&err) }()
		replayed, idemErr := op.beginIdempotency(map[string]string{
			"replicas": fmt.Sprintf("%d", scaleReplicas),
		})
		if idemErr != nil {
			return idemErr
		}
		if replayed {
			return nil
		}

		from, to, err := scaleApplication(context.Background(), appInfo, state, scaleReplicas)
		if err != nil {
			phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "scale", false, err.Error(), 0, "", "")
			return err
		}
		reconcileAppWithDeploy(appInfo.Name)
		phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "scale", true, "", 0, "", "")

		if from < to {
			fmt.Printf("%s Scaled %s from %d to %d replica(s) (%d launched)\n",
				color.GreenString("✓"), color.CyanString("'%s'", appInfo.Name), from, to, to-from)
		} else {
			fmt.Printf("%s Scaled %s from %d to %d replica(s) (%d drained)\n",
				color.GreenString("✓"), color.CyanString("'%s'", appInfo.Name), from, to, from-to)
		}
		op.setResult(&ops.Result{Strategy: string(state.Mode) + "-scale"})
		return op.writeResultEnv(machine.Success(op.operationID(), scaleResult{
			App:      appInfo.Name,
			AppID:    appInfo.ID,
			From:     from,
			To:       to,
			Replicas: to,
		}))
	},
}

// scaleApplication applies one replica count through the app's existing
// scaling path: WorkerScale for workers, ScaleExecutor for rolling services.
// Callers must not hold the app lock; both paths acquire it themselves.
func scaleApplication(ctx context.Context, appInfo *app.AppInfo, state *deploy.DeployState, replicas int) (int, int, error) {
	switch state.Mode {
	case deploy.ModeWorker:
		sig, grace, err := workerStopSettings(appInfo)
		if err != nil {
			return 0, 0, err
		}
		return deploy.WorkerScale(ctx, appInfo.Name, replicas, sig, grace)
	case deploy.ModeRolling:
		client := autoscaleProxyClient()
		if client == nil {
			return 0, 0, phelixerr.New(phelixerr.CodeConfiguration,
				"the proxy control socket is unavailable; start it with 'phelix proxy' before scaling a rolling service")
		}
		from := len(state.Replicas)
		executor := &deploy.ScaleExecutor{
			AppName:        appInfo.Name,
			AppID:          appInfo.ID,
			PublicPort:     state.PublicPort,
			Launcher:       deploy.LauncherForApp(appInfo.Name),
			ProxyClient:    client,
			HealthProvider: deploy.DefaultHealthProvider(),
			GracePeriod:    time.Duration(state.GraceSeconds) * time.Second,
		}
		if err := executor.Scale(ctx, from, replicas); err != nil {
			return from, from, err
		}
		return from, replicas, nil
	default:
		return 0, 0, phelixerr.Newf(phelixerr.CodeConfiguration,
			"mode %s has no manual scaling path", state.Mode)
	}
}

// validateScaleRange keeps a manual replica count inside the configured
// autoscaling bounds when autoscaling is enabled for the app.
func validateScaleRange(dir string, replicas int) error {
	cfg, err := project.Load(dir)
	if err != nil && phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
		return err
	}
	if cfg == nil || cfg.Deploy == nil || cfg.Deploy.Autoscaling == nil {
		return nil
	}
	settings := cfg.Deploy.Autoscaling.Resolve(cfg.Deploy.Replicas)
	if !settings.Enabled {
		return nil
	}
	if replicas < settings.MinReplicas || replicas > settings.MaxReplicas {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"replicas %d is outside the configured autoscaling range %d..%d", replicas, settings.MinReplicas, settings.MaxReplicas)
	}
	return nil
}

// scaleResult is the machine-contract result of a successful scale.
type scaleResult struct {
	App      string `json:"app"`
	AppID    string `json:"app_id"`
	From     int    `json:"from_replicas"`
	To       int    `json:"to_replicas"`
	Replicas int    `json:"replicas"`
}

func init() {
	ScaleCmd.Flags().IntVarP(&scaleReplicas, "replicas", "n", 0, "Desired replica count (>= 1)")
	ScaleCmd.Flags().BoolVar(&scaleJSON, "json", false,
		"Output machine-readable JSON (stdout carries only the result envelope; progress moves to stderr)")
	ScaleCmd.Flags().StringVar(&scaleRequestKey, "request-key", "",
		"Idempotency key: retrying with the same key and same inputs replays the recorded result")
}
