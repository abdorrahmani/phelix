package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/workspace"
)

func TestOrchestrateWorkspaceRollback_ReverseDependencyOrder(t *testing.T) {
	workspaceTestEnv(t, `
services:
  auth: { path: ./auth, port: 8081 }
  orders: { path: ./orders, port: 8082, depends_on: [auth] }
  gateway: { path: ./gateway, port: 8083, depends_on: [orders] }
`, "auth", "orders", "gateway")
	seedApps(t, "auth", "orders", "gateway")

	rec := &sequentialRecorder{}
	rollbackOne := func(svc *workspace.Service, _ *app.AppInfo, _ int, _ string) (int, int, error) {
		rec.record(svc.Name)
		return 3, 2, nil
	}
	if err := orchestrateWorkspaceRollback(rollbackOne, "test"); err != nil {
		t.Fatalf("orchestrateWorkspaceRollback: %v", err)
	}
	got := rec.order()
	want := []string{"gateway", "orders", "auth"}
	if len(got) != len(want) {
		t.Fatalf("rollback order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rollback order = %v, want consumers reverted before their dependencies %v", got, want)
		}
	}

	state, err := workspace.LoadState("mesh")
	if err != nil || state == nil {
		t.Fatalf("workspace state not written: %v", err)
	}
	for _, svc := range state.Services {
		if svc.LastOperation != "rollback" || svc.LastResult != "succeeded" || svc.LastVersion != 2 {
			t.Fatalf("state for %s = %+v, want a succeeded rollback to v2", svc.Name, svc)
		}
	}
}

func TestOrchestrateWorkspaceRollback_HaltsAndSkipsRemaining(t *testing.T) {
	workspaceTestEnv(t, `
services:
  auth: { path: ./auth, port: 8081 }
  orders: { path: ./orders, port: 8082, depends_on: [auth] }
  gateway: { path: ./gateway, port: 8083, depends_on: [orders] }
`, "auth", "orders", "gateway")
	seedApps(t, "auth", "orders", "gateway")

	rec := &sequentialRecorder{}
	rollbackOne := func(svc *workspace.Service, _ *app.AppInfo, _ int, _ string) (int, int, error) {
		rec.record(svc.Name)
		if svc.Name == "orders" {
			return 3, 0, fmt.Errorf("no previous version for %s", svc.Name)
		}
		return 3, 2, nil
	}
	err := orchestrateWorkspaceRollback(rollbackOne, "test")
	if err == nil || !strings.Contains(err.Error(), "orders") {
		t.Fatalf("err = %v, want a halt naming orders", err)
	}
	got := rec.order()
	if len(got) != 2 || got[0] != "gateway" || got[1] != "orders" {
		t.Fatalf("rollback order = %v, want auth skipped after orders failed", got)
	}
	state, err := workspace.LoadState("mesh")
	if err != nil || state == nil {
		t.Fatalf("workspace state not written: %v", err)
	}
	for _, svc := range state.Services {
		switch svc.Name {
		case "orders":
			if svc.LastResult != "failed" || svc.LastError == "" {
				t.Fatalf("orders state = %+v, want a recorded failure", svc)
			}
		case "auth":
			if svc.LastOperation != "" {
				t.Fatalf("auth state = %+v, want no rollback attempt", svc)
			}
		}
	}
}

func TestOrchestrateWorkspaceRollback_RequiresRegisteredService(t *testing.T) {
	workspaceTestEnv(t, `
services:
  auth: { path: ./auth, port: 8081 }
`, "auth")
	seedApps(t)

	rec := &sequentialRecorder{}
	rollbackOne := func(svc *workspace.Service, _ *app.AppInfo, _ int, _ string) (int, int, error) {
		rec.record(svc.Name)
		return 2, 1, nil
	}
	err := orchestrateWorkspaceRollback(rollbackOne, "test")
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("err = %v, want a not-registered failure", err)
	}
	if len(rec.order()) != 0 {
		t.Fatalf("no service should roll back when none is registered, got %v", rec.order())
	}
}
