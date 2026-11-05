package secrets

import (
	"context"
	"testing"
)

func TestSecurityStateIsPerApp(t *testing.T) {
	testEnv(t)
	badApp := "3f0e1d2c-3b4a-4958-8677-8899aabbccdd"
	goodApp := "4f0e1d2c-3b4a-4958-8677-8899aabbccdd"

	if state, _ := SecurityOf(badApp); state != SecurityHealthy {
		t.Fatalf("an unevaluated app must be healthy, got %v", state)
	}

	if _, err := Set(badApp, "db", "value"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := ReplaceBindings(badApp, []Binding{{Secret: "ghost", Env: "GHOST"}}); err == nil {
		t.Fatal("missing import was accepted")
	}
	if _, err := OverlayForLaunch(context.Background(), badApp, []Binding{{Secret: "ghost", Env: "GHOST"}}); err == nil {
		t.Fatal("the broken app must fail to resolve")
	}
	if state, reason := SecurityOf(badApp); state != SecurityDegraded || reason == "" {
		t.Fatalf("the broken app must be degraded with a reason, got %v %q", state, reason)
	}

	if state, _ := SecurityOf(goodApp); state != SecurityHealthy {
		t.Fatalf("one app's failure must not degrade others, got %v", state)
	}

	if _, err := Set(goodApp, "ok", "fine"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := OverlayForLaunch(context.Background(), goodApp, []Binding{{Secret: "ok", Env: "OK"}}); err != nil {
		t.Fatalf("healthy app overlay: %v", err)
	}
	if state, _ := SecurityOf(goodApp); state != SecurityHealthy {
		t.Fatalf("the healthy app must stay healthy, got %v", state)
	}

	if err := ReplaceBindings(badApp, []Binding{{Secret: "db", Env: "DB"}}); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if _, err := OverlayForLaunch(context.Background(), badApp, []Binding{{Secret: "db", Env: "DB"}}); err != nil {
		t.Fatalf("recovery overlay: %v", err)
	}
	if state, _ := SecurityOf(badApp); state != SecurityHealthy {
		t.Fatalf("a successful resolution must clear the degraded state, got %v", state)
	}
	ReleaseCoverage(goodApp)
	ReleaseCoverage(badApp)
}
