package cmd

import (
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"

	"github.com/abdorrahmani/phelix/internal/deploy"
)

// Tests for the --auto-rollback CLI surface: flag registration and the
// single-trigger classification helper.

// TestRebuildAutoRollbackFlagRegistered: --auto-rollback exists on rebuild,
// defaults to off, and is documented (production UX requirement).
func TestRebuildAutoRollbackFlagRegistered(t *testing.T) {
	flag := RebuildCmd.Flags().Lookup("auto-rollback")
	if flag == nil {
		t.Fatal("--auto-rollback flag is not registered on rebuild")
	}
	if flag.DefValue != "false" {
		t.Fatalf("auto-rollback default = %s, want false (opt-in)", flag.DefValue)
	}
	if !strings.Contains(flag.Usage, "previous known-good version") {
		t.Fatalf("flag usage %q should explain the recovery behavior", flag.Usage)
	}
}

// TestRollbackVerifyFlagRegistered: --verify exists on rollback, defaults off.
func TestRollbackVerifyFlagRegistered(t *testing.T) {
	flag := RollbackCmd.Flags().Lookup("verify")
	if flag == nil {
		t.Fatal("--verify flag is not registered on rollback")
	}
	if flag.DefValue != "" {
		t.Fatalf("verify default = %q, want empty (opt-in)", flag.DefValue)
	}
}

// TestRecoverableDeployFailureClassified (cmd-level mirror of the deploy test,
// pinned so a classifier change breaks loudly at the CLI boundary too).
func TestRecoverableDeployFailureClassified(t *testing.T) {
	if !deploy.RecoverableDeployFailure(phelixerr.New(phelixerr.CodeHealthCheckFailed, "unhealthy")) {
		t.Fatal("deploy-phase failure must be recoverable")
	}
	if deploy.RecoverableDeployFailure(phelixerr.New(phelixerr.CodeBuildFailed, "compile error")) {
		t.Fatal("build failure must not trigger recovery")
	}
}
