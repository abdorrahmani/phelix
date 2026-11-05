package cmd

import (
	"context"

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
		outcome, err := deploy.ReconcileSecretTxn(info.Name, info.ID)
		switch {
		case err != nil:
			logs.Warning("secrets", "startup reconciliation for %s: %v", info.Name, err)
		case outcome == deploy.SecretTxnCommitted:
			logs.Info("secrets", "settled pending transaction for %s: committed", info.Name)
		case outcome == deploy.SecretTxnDiscarded:
			logs.Info("secrets", "settled pending transaction for %s: discarded", info.Name)
		}
		bindings, err := secrets.CommittedBindings(info.ID)
		if err != nil {
			logs.Warning("secrets", "cannot read the secret store of %s: %v", info.Name, err)
			continue
		}
		if len(bindings) == 0 {
			continue
		}
		if _, err := secrets.OverlayForLaunch(context.Background(), info.ID, bindings); err != nil {
			continue
		}
		logs.Info("secrets", "registered %d bound values for %s", len(bindings), info.Name)
	}
}
