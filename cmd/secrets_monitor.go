package cmd

import (
	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/secrets"
)

// startupSecretsRuntime settles every pending secret transaction against the
// deployment journal and registers every application's bound values with the
// redaction layer before the daemon serves anything. Failure is scoped to
// one application: that application enters the degraded security state
// (its launches fail and its log streams are withheld) while every other
// application continues normally.
func startupSecretsRuntime() {
	apps := make([]*app.AppInfo, 0, len(app.Manager.(*app.AppManager).Apps))
	for _, info := range app.Manager.(*app.AppManager).Apps {
		apps = append(apps, info)
	}
	for _, info := range apps {
		release, err := deploy.AcquireDeployLock(info.Name, "secrets-recovery")
		if err != nil {
			if bindings, bindingErr := secrets.CommittedBindings(info.ID); bindingErr != nil || len(bindings) > 0 || len(info.SecretEnvs) > 0 {
				secrets.MarkDegraded(info.ID, "secret coverage awaits the active deployment")
			}
			continue
		}
		outcome, err := deploy.ReconcileSecretTxn(info.Name, info.ID)
		release()
		switch {
		case err != nil:
			logs.Warning("secrets", "startup reconciliation for %s: %v", info.Name, err)
		case outcome == deploy.SecretTxnCommitted:
			logs.Info("secrets", "settled pending transaction for %s: committed", info.Name)
		case outcome == deploy.SecretTxnDiscarded:
			logs.Info("secrets", "settled pending transaction for %s: discarded", info.Name)
		}
		_ = deploy.RefreshSecretCoverage(app.AppListItem{ID: info.ID, Name: info.Name, PID: info.PID, Status: info.Status, SecretEnvs: info.SecretEnvs})
	}
}
