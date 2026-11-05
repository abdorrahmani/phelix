package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/secrets"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/spf13/cobra"
)

var restartReplica string

var RestartCmd = &cobra.Command{
	Use:   "restart [ID|AppName]",
	Short: "Restart the application by its ID or AppName. Workers accept --replica N.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "missing required <ID|AppName>; usage: phelix restart <ID|AppName>")
			}
			identifier, err := PromptApp(false, "Select application to restart")
			if err != nil {
				return err
			}
			args = []string{identifier}
		}

		identifier := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
		}

		appInfo, err := GetAppInfo(identifier)
		if err != nil {
			return err
		}

		return restartAppInfo(appInfo)
	},
}

func init() {
	RestartCmd.Flags().StringVar(&restartReplica, "replica", "", "Restart only this worker replica slot (e.g. 0, 1); worker apps only")
}

// restartAppInfo restarts an application with exactly the semantics of
// `phelix restart`: worker replicas, deployment-aware stop and start for
// blue-green and rolling apps, or a plain process restart for classic apps.
// Every secrets command that needs to bring new values into a running
// application uses the same helper, so restart behavior never diverges.
func restartAppInfo(appInfo *app.AppInfo) error {
	if !app.ValidAppID(appInfo.ID) {
		return restartAppInfoLegacy(appInfo)
	}
	store, err := secrets.Open(appInfo.ID)
	if err != nil {
		return err
	}
	if len(store.Bindings) == 0 && store.Pending == nil && len(appInfo.SecretEnvs) == 0 {
		state, err := deploy.Load(appInfo.Name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		hasRuntimeSecrets := false
		if state != nil {
			for _, instances := range []map[string]*deploy.Instance{state.Slots, state.Replicas} {
				for _, inst := range instances {
					if inst != nil && len(inst.SecretEnvs) > 0 {
						hasRuntimeSecrets = true
					}
				}
			}
		}
		if !hasRuntimeSecrets {
			return restartAppInfoLegacy(appInfo)
		}
	}
	release, err := deploy.AcquireDeployLock(appInfo.Name, "restart")
	if err != nil {
		return err
	}
	defer release()
	err = restartAppInfoLocked(appInfo)
	status := secrets.ApplyRestartSucceeded
	if restartReplica != "" {
		status = secrets.ApplyRestartPartial
	}
	if err != nil {
		status = secrets.ApplyRestartFailed
	}
	return errors.Join(err, recordApply(appInfo.ID, status))
}

// restartAppInfoLocked shares secret restart behavior with set and apply.
func restartAppInfoLocked(appInfo *app.AppInfo) error {
	if _, err := deploy.ReconcileSecretTxn(appInfo.Name, appInfo.ID); err != nil {
		return err
	}
	state, err := deploy.Load(appInfo.Name)
	if err == nil && state.Mode != "classic" {
		bindings, err := secrets.CommittedBindings(appInfo.ID)
		if err != nil {
			return err
		}
		desired, err := secrets.PinBindings(context.Background(), appInfo.ID, bindings)
		if err != nil {
			return err
		}
		defer desired.Close()
		var worker *deploy.Worker
		if state.Mode == deploy.ModeWorker {
			cfg, err := loadProjectConfigFrom(appInfo.Directory)
			if err != nil {
				return err
			}
			worker, err = buildWorkerEngine(appInfo, appInfo.Name, cfg, nil, len(state.Replicas), "", nil, &colorLogger{})
			if err != nil {
				return err
			}
		}
		if err := deploy.RestartSecretInstances(context.Background(), state, desired, worker, restartReplica); err != nil {
			return err
		}
		reconcileAppWithDeploy(appInfo.Name)
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	manager, ok := app.Manager.(*app.AppManager)
	if !ok {
		return app.Manager.RestartApplication(appInfo.ID)
	}
	bindings, err := secrets.CommittedBindings(appInfo.ID)
	if err != nil {
		return err
	}
	txn, err := deploy.BeginSecretTxn(appInfo.ID, appInfo.Name, "", bindings, deploy.RuntimeNative)
	if err != nil {
		return err
	}
	restartErr := manager.RestartApplicationWithSecrets(appInfo.ID, txn.Set, func(current *app.AppInfo, accepted bool) error {
		version := 0
		if state != nil {
			version = state.ActiveVersion
		}
		journal := &deploy.DeployState{AppName: appInfo.Name, AppID: appInfo.ID, Mode: "classic", PublicPort: appInfo.Port, ActiveVersion: version}
		if current != nil {
			journal.Slots = map[string]*deploy.Instance{"classic": {PID: current.PID, StartTicks: current.StartTicks, Port: current.Port, SecretEnvs: current.SecretEnvs, Status: "running"}}
		}
		if accepted {
			txn.Stamp(journal)
		}
		return deploy.Store(journal)
	})
	return errors.Join(restartErr, deploy.FinishSecretTxn(appInfo.ID, txn, restartErr))
}

func restartAppInfoLegacy(appInfo *app.AppInfo) error {
	if wstate, werr := workerAppState(appInfo.Name); werr == nil && wstate != nil {
		fmt.Printf("• Restarting worker '%s' (ID: %s)\n", appInfo.Name, appInfo.ID)
		if err := runWorkerRestart(appInfo, restartReplica); err != nil {
			phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "restart", false, err.Error(), 0, "", "")
			return phelixerr.Wrapf(phelixerr.CodeProcessFailed, err,
				"failed to restart worker replicas for %q (ID: %s)", appInfo.Name, appInfo.ID)
		}
		phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "restart", true, "", 0, "", "")
		return nil
	}

	state, derr := loadDeployState(appInfo.Name)
	if derr != nil {
		return derr
	}
	if state != nil {
		fmt.Printf("• Restarting deployment for '%s' (ID: %s)\n", appInfo.Name, appInfo.ID)
		if err := runDeployAwareStop(appInfo); err != nil {
			phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "restart", false, err.Error(), 0, "", "")
			return phelixerr.Wrapf(
				phelixerr.CodeProcessFailed,
				err,
				"failed to stop deployment for %q (ID: %s)",
				appInfo.Name, appInfo.ID,
			)
		}
		if err := runDeployAwareStart(appInfo); err != nil {
			phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "restart", false, err.Error(), 0, "", "")
			return phelixerr.Wrapf(
				phelixerr.CodeProcessFailed,
				err,
				"failed to restore deployment for %q (ID: %s)",
				appInfo.Name, appInfo.ID,
			)
		}
		fmt.Printf("✓ Application '%s' (ID: %s) restarted successfully\n", appInfo.Name, appInfo.ID)
		phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "restart", true, "", 0, "", "")
		return nil
	}

	fmt.Printf("• Restarting Application '%s' (ID: %s)\n", appInfo.Name, appInfo.ID)
	if err := app.Manager.RestartApplication(appInfo.ID); err != nil {
		phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "restart", false, err.Error(), 0, "", "")
		return phelixerr.Wrapf(
			phelixerr.CodeProcessFailed,
			err,
			"failed to restart application %q (ID: %s)",
			appInfo.Name,
			appInfo.ID,
		)
	}

	fmt.Printf("✓ Application '%s' (ID: %s) restarted successfully\n", appInfo.Name, appInfo.ID)
	phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "restart", true, "", 0, "", "")
	return nil
}
