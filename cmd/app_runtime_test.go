package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
)

func installStatusDocker(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
inspect)
  if [ "$2" = "--format" ]; then
    if [ "$3" = '{{json .Config.Labels}}' ]; then
      if [ "$STATUS_DOCKER_LEGACY" = true ]; then
        printf '%s\n' '{"phelix.managed":"true","phelix.app":"runtime-test"}'
      else
        printf '%s\n' '{"phelix.managed":"true","phelix.controller":"runtime-controller"}'
      fi
    elif [ "$4" = dead ] || [ "$STATUS_DOCKER_STOPPED" = true ]; then
      printf '%s\n' 'false|true'
    else
      printf '%s\n' 'true|true'
    fi
  else
    printf '%s\n' '[{"Id":"live","State":{"Status":"running"}}]'
  fi
  ;;
stats)
  if [ "$STATUS_DOCKER_METRICS_FAIL" = true ]; then exit 1; fi
  printf '%s\n' '{"Container":"live","CPUPerc":"12.5%","MemUsage":"14MiB / 1GiB","NetIO":"0B / 0B","BlockIO":"0B / 0B"}'
  ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PHELIX_CONTROLLER_ID", "runtime-controller")
}

func TestContainerStatusSurvivesStateReloads(t *testing.T) {
	for _, mode := range []deploy.Mode{deploy.ModeBlueGreen, deploy.ModeRolling, deploy.ModeWorker} {
		t.Run(string(mode), func(t *testing.T) {
			t.Setenv("PHELIX_DATA_DIR", t.TempDir())
			installStatusDocker(t)
			withJobManager(t, map[string]*app.AppInfo{
				"runtime-id": {ID: "runtime-id", Name: "runtime-test", Status: "stopped", Watching: true},
			})
			started := time.Now().Add(-5 * time.Minute)
			instance := &deploy.Instance{Slot: "0", ContainerID: "live", PID: 99999999, StartedAt: started, Status: "running"}
			state := &deploy.DeployState{AppName: "runtime-test", Mode: mode, Runtime: deploy.RuntimeDocker, PublicPort: 8086}
			if mode == deploy.ModeBlueGreen {
				state.ActiveSlot = deploy.SlotGreen
				instance.Slot = deploy.SlotGreen
				state.Slots = map[string]*deploy.Instance{deploy.SlotGreen: instance, deploy.SlotBlue: {ContainerID: "dead"}}
			} else {
				state.Replicas = map[string]*deploy.Instance{"0": instance}
			}
			if err := deploy.Store(state); err != nil {
				t.Fatal(err)
			}
			for _, legacy := range []string{"false", "true"} {
				t.Setenv("STATUS_DOCKER_LEGACY", legacy)
				reconcileAppWithDeploy("runtime-id")
				for i := 0; i < 2; i++ {
					items := app.Manager.ListApplications()
					if len(items) != 1 || items[0].Status != "running" || items[0].PID != instance.PID || items[0].Uptime == "N/A" || !items[0].Watching {
						t.Fatalf("list lost container status: %+v", items)
					}
					for _, identifier := range []string{"runtime-id", "runtime-test"} {
						status, err := app.Manager.StatusApplication(identifier)
						if err != nil {
							t.Fatal(err)
						}
						if status.Status != "running" || status.PID != instance.PID || status.Uptime == "N/A" || status.RAMUsage != 14<<20 || status.CPUUsage != 12.5 {
							t.Fatalf("incorrect container status: %+v", status)
						}
					}
				}
			}
			t.Setenv("STATUS_DOCKER_METRICS_FAIL", "true")
			if _, err := app.Manager.StatusApplication("runtime-id"); err == nil {
				t.Fatal("missing Docker metrics were reported as zero")
			}
			t.Setenv("STATUS_DOCKER_STOPPED", "true")
			status, err := app.Manager.StatusApplication("runtime-id")
			if err != nil || status.Status != "stopped" || status.PID != 0 || status.Uptime != "N/A" || status.RAMUsage != 0 {
				t.Fatalf("stopped container retained live status: %+v, %v", status, err)
			}
		})
	}
}

func TestContainerReplicaStatusAndUptime(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	installStatusDocker(t)
	withJobManager(t, map[string]*app.AppInfo{"runtime-id": {ID: "runtime-id", Name: "runtime-test"}})
	started := time.Now().Add(-10 * time.Minute)
	state := &deploy.DeployState{AppName: "runtime-test", Mode: deploy.ModeRolling, Runtime: deploy.RuntimeDocker,
		Replicas: map[string]*deploy.Instance{
			"0": {Slot: "0", ContainerID: "live", StartedAt: started},
			"1": {Slot: "1", ContainerID: "dead", PID: 99999998},
		},
	}
	if err := deploy.Store(state); err != nil {
		t.Fatal(err)
	}
	status, err := app.Manager.StatusApplication("runtime-id")
	if err != nil || status.Status != "degraded" || !strings.HasPrefix(status.Uptime, "00h 10m") || status.RAMUsage != 14<<20 {
		t.Fatalf("incorrect degraded status: %+v, %v", status, err)
	}
	state.Replicas["1"].ContainerID = "live"
	if err := deploy.Store(state); err != nil {
		t.Fatal(err)
	}
	status, err = app.Manager.StatusApplication("runtime-id")
	if err != nil || status.Status != "running" || !strings.HasPrefix(status.Uptime, "00h 10m") {
		t.Fatalf("first container start time was replaced: %+v, %v", status, err)
	}
	state.Replicas["0"].StartedAt = time.Time{}
	if err := deploy.Store(state); err != nil {
		t.Fatal(err)
	}
	status, err = app.Manager.StatusApplication("runtime-id")
	if err != nil || status.Status != "running" || status.Uptime != "N/A" {
		t.Fatalf("unknown start time produced an uptime: %+v, %v", status, err)
	}
}

func TestObservedDockerDeploymentPersistsRecoveryIntent(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	installStatusDocker(t)
	withJobManager(t, map[string]*app.AppInfo{
		"runtime-id": {ID: "runtime-id", Name: "runtime-test", Status: "stopped"},
	})
	if err := app.Manager.SaveState(); err != nil {
		t.Fatal(err)
	}
	state := &deploy.DeployState{AppName: "runtime-test", Mode: deploy.ModeBlueGreen, Runtime: deploy.RuntimeDocker,
		ActiveSlot: deploy.SlotBlue, PublicPort: 8080,
		Slots: map[string]*deploy.Instance{deploy.SlotBlue: {Slot: deploy.SlotBlue, ContainerID: "live", PID: 99999999, Status: "running", StartedAt: time.Now()}},
	}
	if err := deploy.Store(state); err != nil {
		t.Fatal(err)
	}
	reconcileAppWithDeploy("runtime-test")
	if err := app.Manager.LoadState(); err != nil {
		t.Fatal(err)
	}
	info, err := GetAppInfo("runtime-test")
	if err != nil || !info.AutoStart {
		t.Fatal("observed running status prevented recovery intent from being saved", info, err)
	}
}
