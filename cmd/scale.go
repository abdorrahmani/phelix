package cmd

import (
	"context"
	"fmt"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/authz"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var scaleReplicas int
var scaleJSON bool
var scaleRequestKey string

var ScaleCmd = &cobra.Command{
	Use:           "scale [ID|AppName] --replicas N",
	Short:         "Change a worker's replica count in place, without rebuilding.",
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
				"missing required --replicas N (the desired worker replica count, >= 1)")
		}
		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "missing required <ID|AppName>; usage: phelix scale <ID|AppName> --replicas N")
			}
			identifier, err := PromptApp(false, "Select worker to scale")
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
		if wstate, werr := workerAppState(appInfo.Name); werr != nil || wstate == nil {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"%s is not a worker app\nHint: phelix scale changes the replica set of a type: worker app; services scale via 'phelix rebuild --replicas N'", appInfo.Name)
		}

		if authzErr := authorizeDirectExecution(authz.ActionRebuild, appInfo.Name, appInfo.ID); authzErr != nil {
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

		sig, grace, err := workerStopSettings(appInfo)
		if err != nil {
			return err
		}
		from, to, err := deploy.WorkerScale(context.Background(), appInfo.Name, scaleReplicas, sig, grace)
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
		op.setResult(&ops.Result{Strategy: "worker-scale"})
		return op.writeResultEnv(machine.Success(op.operationID(), scaleResult{
			App:      appInfo.Name,
			AppID:    appInfo.ID,
			From:     from,
			To:       to,
			Replicas: to,
		}))
	},
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
