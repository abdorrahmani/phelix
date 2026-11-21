package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/resources"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/supplychain"
)

func TestSecurityReviewLegacyRollbackLauncherEnforcesPolicy(t *testing.T) {
	withTempHome(t)
	appName := "security-review-legacy"
	dir, err := versionDir(appName, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "binary")
	if err := os.WriteFile(path, []byte("legacy-artifact"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveVersions(appName, &VersionsFile{Versions: []VersionMeta{{Version: 1}}}); err != nil {
		t.Fatal(err)
	}
	p := supplychain.DefaultPolicy()
	p.Mode = "enforce"
	p.RequireSignature = true
	p.TrustedIdentity = "review@example.com"
	p.TrustedIssuer = "https://issuer.example.com"
	if err := supplychain.NewStore(server.DataDir()).SavePolicy(appName, p); err != nil {
		t.Fatal(err)
	}
	source := ExistingVersionSource{AppName: appName, Version: 1}
	binary, _, err := source.Build(context.Background())
	if err != nil {
		if strings.Contains(err.Error(), "security metadata is missing") {
			return
		}
		t.Fatal(err)
	}
	proc, port, err := launchAppInstance(context.Background(), binary, nil, resources.Config{}, appName)
	if err == nil || !strings.Contains(err.Error(), "security metadata is missing") || proc != nil || port != 0 {
		t.Fatalf("legacy rollback did not enforce signature policy before launch: proc=%v port=%d err=%v", proc, port, err)
	}
}
