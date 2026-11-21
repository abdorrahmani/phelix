package deploy

import (
	"context"
	"fmt"
	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/supplychain"
)

func verifyNativeAppArtifact(ctx context.Context, appName, binaryPath string) error {
	appID := appName
	for _, info := range app.Manager.ListApplications() {
		if info.Name == appName {
			appID = info.ID
			break
		}
	}
	return supplychain.VerifyApplicationPath(ctx, supplychain.NewStore(server.DataDir()), binaryPath, appID, appName)
}

func checkContainerSecurityPolicy(appName string) error {
	appID := appName
	for _, info := range app.Manager.ListApplications() {
		if info.Name == appName {
			appID = info.ID
			break
		}
	}
	policy, err := supplychain.NewStore(server.DataDir()).LoadPolicy(appID)
	if err != nil {
		return err
	}
	if policy.Mode == "enforce" {
		return fmt.Errorf("container artifact security is unavailable for %s; an enforce policy cannot accept an unverified image", appName)
	}
	return nil
}
