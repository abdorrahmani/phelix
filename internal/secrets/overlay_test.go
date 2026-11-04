package secrets

import (
	"context"
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
		overlay, err := OverlayForLaunch(context.Background(), testAppID, bindings)
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
		overlay, err := OverlayForLaunch(context.Background(), testAppID, bindings)
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
	if _, err := OverlayForLaunch(context.Background(), testAppID, bindings); err != nil {
		t.Fatalf("overlay: %v", err)
	}
	rendered := phelixerr.Redact("app said: token " + value + " done")
	if strings.Contains(rendered, value) {
		t.Fatalf("resolved value must be masked after launch registration: %q", rendered)
	}
	ReleaseCoverage(testAppID)
	rendered = phelixerr.Redact("app said: token " + value + " done")
	if !strings.Contains(rendered, value) {
		t.Fatalf("released coverage must stop masking: %q", rendered)
	}
}

func TestCoverageRetainsBoundedHistory(t *testing.T) {
	testEnv(t)
	releases := make([]string, 0, overlayGenerations+1)
	for gen := 0; gen < overlayGenerations+1; gen++ {
		value := "generation-value-" + string(rune('a'+gen))
		releases = append(releases, value)
		if _, err := Set(testAppID, "gen", value); err != nil {
			t.Fatalf("set: %v", err)
		}
		bindings := []Binding{{Secret: "gen", Env: "GEN"}}
		if _, err := OverlayForLaunch(context.Background(), testAppID, bindings); err != nil {
			t.Fatalf("overlay: %v", err)
		}
	}
	oldest := releases[0]
	rendered := phelixerr.Redact("log with " + oldest)
	if !strings.Contains(rendered, oldest) {
		t.Fatalf("after %d further generations the oldest value must be released: %q", overlayGenerations, rendered)
	}
	current := releases[len(releases)-1]
	rendered = phelixerr.Redact("log with " + current)
	if strings.Contains(rendered, current) {
		t.Fatalf("the current generation must stay masked: %q", rendered)
	}
	ReleaseCoverage(testAppID)
	if got := phelixerr.Redact("log with " + current); !strings.Contains(got, current) {
		t.Fatalf("release must unmask everything for the app: %q", got)
	}
}

func TestClassicEnvOverlay(t *testing.T) {
	testEnv(t)
	t.Run("app without bindings gets nothing", func(t *testing.T) {
		overlay, err := ClassicEnvOverlay(context.Background(), testAppID)
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
		overlay, err := ClassicEnvOverlay(context.Background(), testAppID)
		if err != nil {
			t.Fatalf("classic overlay: %v", err)
		}
		if len(overlay) != 1 || overlay[0] != "REDIS_PASSWORD=redis-secret-1" {
			t.Fatalf("overlay = %v", overlay)
		}
		ReleaseCoverage(testAppID)
	})
	t.Run("missing declared secret fails closed", func(t *testing.T) {
		if err := ReplaceBindings(testAppID, []Binding{{Secret: "gone", Env: "GONE"}}); err != nil {
			t.Fatalf("bindings: %v", err)
		}
		_, err := ClassicEnvOverlay(context.Background(), testAppID)
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
