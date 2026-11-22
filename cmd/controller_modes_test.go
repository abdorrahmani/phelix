package cmd

import (
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/controller"
	"github.com/abdorrahmani/phelix/internal/deploy"
)

func TestControllerModePreservesNativeApplications(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	withJobManager(t, map[string]*app.AppInfo{
		"native": {ID: "native", Name: "native", AutoStart: true, Status: "stopped"},
	})
	if err := validateControllerApps(controller.Config{Mode: "host"}); err != nil {
		t.Fatal(err)
	}
	if err := validateControllerApps(controller.Config{Mode: "container"}); err == nil {
		t.Fatal("native application was accepted for container recovery")
	}
	info, err := GetAppInfo("native")
	if err != nil || !info.AutoStart || info.Status != "stopped" {
		t.Fatal("validation changed the application", info, err)
	}
	state := &deploy.DeployState{AppName: "native", Runtime: deploy.RuntimeDocker, Mode: deploy.ModeBlueGreen}
	if err := deploy.Store(state); err != nil {
		t.Fatal(err)
	}
	if err := validateControllerApps(controller.Config{Mode: "container"}); err != nil {
		t.Fatal("Docker application was rejected", err)
	}
}
