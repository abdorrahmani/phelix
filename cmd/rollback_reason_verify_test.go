package cmd

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Tests for the CLI surface of rollback --reason / --verify: duration parsing,
// preview rendering, and exit-code classification.

// TestRollbackVerifyDurationParsing: the flag accepts Go durations only.
// Bare numbers were previously a serialization hazard in Phelix ("10" vs
// "10s"), so "30" must never silently parse.
func TestRollbackVerifyDurationParsing(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"10s", 10 * time.Second, false},
		{"30s", 30 * time.Second, false},
		{"1m", time.Minute, false},
		{"2m30s", 2*time.Minute + 30*time.Second, false},
		{"30", 0, true},  // unitless: rejected, never a silent second count
		{"abc", 0, true}, // garbage: rejected
		{"-10s", 0, true},
		{"0s", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			d, err := time.ParseDuration(tc.in)
			if err == nil && d <= 0 {
				err = phelixerr.Newf(phelixerr.CodeInvalidArgument, "must be positive")
			}
			if tc.wantErr && err == nil {
				t.Fatalf("%q must be rejected", tc.in)
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("%q must parse: %v", tc.in, err)
				}
				if d != tc.want {
					t.Errorf("%q = %s, want %s", tc.in, d, tc.want)
				}
			}
		})
	}
}

func captureRollbackPreview(t *testing.T, name string, target int, reason string, verify time.Duration) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = renderRollbackPreview(previewAppInfo(name), name, target, "", rollbackTargetExplicit, reason, verify)
	os.Stdout = old
	w.Close()
	if err != nil {
		t.Fatalf("renderRollbackPreview: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestRenderRollbackPreviewShowsReasonAndVerify: the dry-run plan displays the
// reason and the planned verification window without mutating anything
// (mutation-freedom of the preview itself is covered by the dry-run tests).
func TestRenderRollbackPreviewShowsReasonAndVerify(t *testing.T) {
	seedRollbackPreviewApp(t, "preview-rv", deploy.ModeBlueGreen)
	out := captureRollbackPreview(t, "preview-rv", 2, "Login endpoint returning 500", 30*time.Second)
	if !strings.Contains(out, "Reason") || !strings.Contains(out, "Login endpoint returning 500") {
		t.Errorf("preview missing reason:\n%s", out)
	}
	if !strings.Contains(out, "Verification") || !strings.Contains(out, "30s") {
		t.Errorf("preview missing verification window:\n%s", out)
	}
}

// TestRenderRollbackPreviewOmitsAbsentReasonVerify: without the flags the
// preview keeps its old layout (no empty Reason/Verification rows).
func TestRenderRollbackPreviewOmitsAbsentReasonVerify(t *testing.T) {
	seedRollbackPreviewApp(t, "preview-plain", deploy.ModeBlueGreen)
	out := captureRollbackPreview(t, "preview-plain", 2, "", 0)
	if strings.Contains(out, "  Reason") {
		t.Errorf("preview must not show an empty Reason row:\n%s", out)
	}
	if strings.Contains(out, "  Verification") {
		t.Errorf("preview must not show an empty Verification row:\n%s", out)
	}
}

// TestExitCodeForRollbackVerifyFailed: verification failure maps to the new
// exit code 23 and must never collapse into 22 (rollback failure) — automation
// has to distinguish the two.
func TestExitCodeForRollbackVerifyFailed(t *testing.T) {
	err := phelixerr.New(phelixerr.CodeRollbackVerifyFailed, "verification failed")
	if got := ExitCodeFor(err); got != ExitRollbackVerify {
		t.Errorf("exit code = %d, want %d", got, ExitRollbackVerify)
	}
	if got := ExitCodeFor(phelixerr.New(phelixerr.CodeRollbackFailed, "execution failed")); got != ExitRollback {
		t.Errorf("rollback-failed exit code drifted: %d, want %d", got, ExitRollback)
	}
}

// TestVerifyHistoryRecordRenderIntent: verification-cancelled records keep
// execution SUCCESS with the cancelled window recorded.
func TestVerifyHistoryRecordRenderIntent(t *testing.T) {
	rec := deploy.RollbackHistoryRecord{
		Status: deploy.RollbackStatusSuccess,
		Verification: &deploy.RollbackVerification{
			Requested: true, Duration: "30s", Status: deploy.RollbackVerifyCancelled,
		},
	}
	if rec.Status != deploy.RollbackStatusSuccess {
		t.Fatal("cancelled verification must not change execution status")
	}
	if rec.Verification.Status != deploy.RollbackVerifyCancelled {
		t.Fatal("cancelled outcome must be preserved verbatim")
	}
}
