package secrets

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestOverlayForLaunchAllOrNothing(t *testing.T) {
	testEnv(t)
	if _, err := Set(testAppID, "db_url", "postgres://real"); err != nil {
		t.Fatalf("set: %v", err)
	}
	t.Run("missing secret fails the whole overlay", func(t *testing.T) {
		bindings := []Binding{
			{Secret: "db_url", Env: "DATABASE_URL"},
			{Secret: "ghost", Env: "GHOST_URL"},
		}
		overlay, releaseOverlay, err := OverlayForLaunch(context.Background(), testAppID, bindings)
		defer releaseOverlay()
		if err == nil {
			t.Fatal("one missing secret must fail the overlay")
		}
		if phelixerr.CodeOf(err) != phelixerr.CodeSecretNotFound {
			t.Fatalf("expected SECRET_NOT_FOUND, got %v", err)
		}
		if overlay != nil {
			t.Fatalf("no partial overlay may be returned, got %v", overlay)
		}
	})
	t.Run("complete bindings resolve in declared order", func(t *testing.T) {
		bindings := []Binding{
			{Secret: "db_url", Env: "DATABASE_URL"},
			{Secret: "db_url", Env: "DB_DSN"},
		}
		overlay, releaseOverlay, err := OverlayForLaunch(context.Background(), testAppID, bindings)
		defer releaseOverlay()
		if err != nil {
			t.Fatalf("overlay: %v", err)
		}
		if len(overlay) != 2 {
			t.Fatalf("overlay = %v", overlay)
		}
		if overlay[0] != "DATABASE_URL=postgres://real" || overlay[1] != "DB_DSN=postgres://real" {
			t.Fatalf("overlay = %v", overlay)
		}
	})
}

func TestOverlayValuesAreMasked(t *testing.T) {
	testEnv(t)
	value := "super-hidden-credential-9"
	if _, err := Set(testAppID, "cred", value); err != nil {
		t.Fatalf("set: %v", err)
	}
	bindings := []Binding{{Secret: "cred", Env: "CRED"}}
	_, releaseOverlay, err := OverlayForLaunch(context.Background(), testAppID, bindings)
	defer releaseOverlay()
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	rendered := phelixerr.Redact("app said: token " + value + " done")
	if strings.Contains(rendered, value) {
		t.Fatalf("resolved value must be masked after launch registration: %q", rendered)
	}
	ReleaseCoverage(testAppID)
	if phelixerr.Redact(value) != "***" {
		t.Fatal("app coverage released an in-flight launch")
	}
	releaseOverlay()
	rendered = phelixerr.Redact("app said: token " + value + " done")
	if !strings.Contains(rendered, value) {
		t.Fatalf("released coverage must stop masking: %q", rendered)
	}
}

func TestCoverageRetainsOwnedLiveValue(t *testing.T) {
	testEnv(t)
	old := "live-old-credential"
	release := phelixerr.AcquireSecretValues([]string{old})
	for i := 0; i < 100; i++ {
		AcquireCoverage(testAppID, []string{fmt.Sprintf("rotation-%d", i)})
	}
	if strings.Contains(phelixerr.Redact(old), old) {
		t.Fatal("live instance lost coverage")
	}
	release()
	ReleaseCoverage(testAppID)
	if phelixerr.Redact(old) != old {
		t.Fatal("released instance retained coverage")
	}
}

func TestClassicEnvOverlay(t *testing.T) {
	testEnv(t)
	t.Run("app without bindings gets nothing", func(t *testing.T) {
		overlay, releaseOverlay, err := ClassicEnvOverlay(context.Background(), testAppID)
		defer releaseOverlay()
		if err != nil {
			t.Fatalf("classic overlay: %v", err)
		}
		if overlay != nil {
			t.Fatalf("expected empty overlay, got %v", overlay)
		}
	})
	t.Run("app with bindings resolves them", func(t *testing.T) {
		if _, err := Set(testAppID, "redis_pass", "redis-secret-1"); err != nil {
			t.Fatalf("set: %v", err)
		}
		if err := ReplaceBindings(testAppID, []Binding{{Secret: "redis_pass", Env: "REDIS_PASSWORD"}}); err != nil {
			t.Fatalf("bindings: %v", err)
		}
		overlay, releaseOverlay, err := ClassicEnvOverlay(context.Background(), testAppID)
		defer releaseOverlay()
		if err != nil {
			t.Fatalf("classic overlay: %v", err)
		}
		if len(overlay) != 1 || overlay[0] != "REDIS_PASSWORD=redis-secret-1" {
			t.Fatalf("overlay = %v", overlay)
		}
		ReleaseCoverage(testAppID)
	})
	t.Run("missing declared secret fails closed", func(t *testing.T) {
		err := ReplaceBindings(testAppID, []Binding{{Secret: "gone", Env: "GONE"}})
		if phelixerr.CodeOf(err) != phelixerr.CodeSecretNotFound {
			t.Fatalf("expected SECRET_NOT_FOUND, got %v", err)
		}
		ReleaseCoverage(testAppID)
	})
}

func TestCommittedBindingsEmptyStore(t *testing.T) {
	testEnv(t)
	bindings, err := CommittedBindings(testAppID)
	if err != nil {
		t.Fatalf("committed bindings: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("expected no bindings, got %v", bindings)
	}
	_ = filepath.Join
}

func TestLaunchResolutionPreservesRuntimeCoverage(t *testing.T) {
	testEnv(t)
	old := "previous-adopted-runtime-credential"
	AcquireCoverage(testAppID, []string{old})
	t.Cleanup(func() { ReleaseCoverage(testAppID); MarkHealthy(testAppID) })
	if _, err := Set(testAppID, "current", "new-launch-credential"); err != nil {
		t.Fatal(err)
	}
	bindings := []Binding{{Secret: "current", Env: "DB"}}
	_, releaseOverlay, err := OverlayForLaunch(context.Background(), testAppID, bindings)
	defer releaseOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if phelixerr.Redact(old) != "***" {
		t.Fatal("launch resolution dropped old instance coverage")
	}
	MarkRuntimeDegraded(testAppID, "runtime inspection failed")
	if _, _, err := OverlayForLaunch(context.Background(), testAppID, bindings); err == nil {
		t.Fatal("launch bypassed degraded runtime coverage")
	}
	if _, err := PinBindings(context.Background(), testAppID, bindings); err == nil {
		t.Fatal("deployment bypassed degraded runtime coverage")
	}
	if state, _ := SecurityOf(testAppID); state != SecurityDegraded {
		t.Fatal("resolution cleared runtime failure")
	}
}
