package matrix

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/builder"
)

func testResults(statuses ...string) []Result {
	results := make([]Result, 0, len(statuses))
	for i, s := range statuses {
		c := Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
		if i > 0 {
			// Give each combination a distinct platform so identities differ.
			c.Arch = "arm64"
			c.Platform = "linux/arm64"
		}
		r := Result{Combination: c, Status: s, Duration: 10 * time.Millisecond}
		if s == "failed" {
			r.Error = errors.New("build failed: token=secret123")
		}
		results = append(results, r)
	}
	return results
}

func TestNewRun_SnapshotsConfiguration(t *testing.T) {
	prof := &Profile{
		Lang:        builder.Go,
		Versions:    []string{"1.26", "1.27"},
		Platforms:   []string{"linux/amd64"},
		Concurrency: 4,
		Source: ProfileSource{
			Lang: SourceDetected, Versions: SourceConfig, Platforms: SourceCLI, Concurrency: SourceConfig,
		},
	}
	started := time.Now()
	run := NewRun("mx_20260909_8f31", "app", "/tmp/proj", prof, started)

	if run.Status != RunStatusRunning {
		t.Fatalf("new run status = %s, want running", run.Status)
	}
	if run.Config.Lang != "go" || run.Config.Concurrency != 4 {
		t.Fatalf("bad snapshot: %+v", run.Config)
	}
	if len(run.Config.Versions) != 2 || run.Config.Versions[0] != "1.26" {
		t.Fatalf("bad snapshot versions: %v", run.Config.Versions)
	}
	if run.Config.Sources.Versions != SourceConfig || run.Config.Sources.Platforms != SourceCLI {
		t.Fatalf("bad snapshot sources: %+v", run.Config.Sources)
	}

	// The snapshot must be a copy: later mutation of the profile cannot
	// rewrite what the run records.
	prof.Versions[0] = "9.99"
	if run.Config.Versions[0] != "1.26" {
		t.Fatal("run snapshot aliases the profile's version slice")
	}
}

func TestRunFinish_Statuses(t *testing.T) {
	cases := []struct {
		name     string
		statuses []string
		want     RunStatus
	}{
		{"all succeeded", []string{"success", "success"}, RunStatusSucceeded},
		{"partial", []string{"success", "failed"}, RunStatusPartial},
		{"all failed", []string{"failed", "failed"}, RunStatusFailed},
		{"single failure", []string{"failed"}, RunStatusFailed},
		{"empty", nil, RunStatusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := NewRun("mx_20260909_8f31", "app", "", &Profile{Lang: builder.Go}, time.Now())
			run.Finish(testResults(tc.statuses...), time.Now())
			if run.Status != tc.want {
				t.Fatalf("status = %s, want %s", run.Status, tc.want)
			}
			if run.Total != len(tc.statuses) {
				t.Fatalf("total = %d, want %d", run.Total, len(tc.statuses))
			}
		})
	}
}

func TestRunFinish_CountersAndIdentity(t *testing.T) {
	run := NewRun("mx_20260909_8f31", "app", "", &Profile{Lang: builder.Go}, time.Now())
	run.Finish(testResults("success", "failed"), time.Now())

	if run.Succeeded != 1 || run.Failed != 1 {
		t.Fatalf("counters: succeeded=%d failed=%d", run.Succeeded, run.Failed)
	}
	if len(run.Combinations) != 2 {
		t.Fatalf("combinations = %d, want 2", len(run.Combinations))
	}
	for i, c := range run.Combinations {
		want := "mx_20260909_8f31/" + c.ID
		if c.Identity != want {
			t.Fatalf("combination %d identity = %q, want %q", i, c.Identity, want)
		}
	}
}

func TestRunFinish_RedactsErrors(t *testing.T) {
	run := NewRun("mx_20260909_8f31", "app", "", &Profile{Lang: builder.Go}, time.Now())
	run.Finish(testResults("failed"), time.Now())
	errText := run.Combinations[0].Error
	if strings.Contains(errText, "secret123") {
		t.Fatalf("secret leaked into run record: %q", errText)
	}
	if errText == "" {
		t.Fatal("error text missing entirely")
	}
}

func TestRunFinish_SkippedCounted(t *testing.T) {
	run := NewRun("mx_20260909_8f31", "app", "", &Profile{Lang: builder.Go}, time.Now())
	run.Finish([]Result{
		{Combination: Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}, Status: "skipped"},
	}, time.Now())
	if run.Skipped != 1 || run.Status != RunStatusSucceeded {
		t.Fatalf("skipped=%d status=%s", run.Skipped, run.Status)
	}
}
