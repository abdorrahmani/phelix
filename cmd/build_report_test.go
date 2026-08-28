package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/buildreport"
	"github.com/abdorrahmani/phelix/internal/matrix"
)

func TestNewNativeBuildReport_AssemblesCapturedData(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "app_1")
	if err := os.WriteFile(bin, make([]byte, 1024*1024+512), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now().Add(-8 * time.Second)
	cfg := &builder.BuildConfig{
		Language:       builder.Go,
		ExtraArgs:      []string{"-trimpath", "-ldflags=-s -w"},
		BuildStartTime: start,
		BuildEndTime:   start.Add(8420 * time.Millisecond),
		Observe: &builder.BuildObservation{
			CacheStatus:     buildreport.CacheCold,
			CacheSource:     buildreport.CacheSourceGoBuild,
			CompilerVersion: "1.27",
		},
	}

	rep := newNativeBuildReport(builder.Go, cfg, bin)
	if rep.Language != "go" || rep.Compiler != "go" || rep.CompilerVersion != "1.27" {
		t.Fatalf("language/toolchain wrong: %+v", rep)
	}
	if rep.DurationMS != 8420 {
		t.Fatalf("duration = %d, want 8420", rep.DurationMS)
	}
	if rep.Cache.Status != buildreport.CacheCold || rep.Cache.Source != buildreport.CacheSourceGoBuild {
		t.Fatalf("cache info wrong: %+v", rep.Cache)
	}
	if rep.Artifact.Type != buildreport.ArtifactBinary || rep.Artifact.SizeBytes != 1024*1024+512 {
		t.Fatalf("artifact wrong: %+v", rep.Artifact)
	}
	if rep.Artifact.Platform == "" {
		t.Fatal("platform must be recorded for native builds")
	}
	if len(rep.BuildArgs) != 2 || rep.BuildArgs[0] != "-trimpath" {
		t.Fatalf("build args not captured: %v", rep.BuildArgs)
	}
}

func TestNewNativeBuildReport_MissingObservationIsSafe(t *testing.T) {
	// A builder run without telemetry must produce an UNKNOWN-cache report,
	// never a fabricated one.
	bin := filepath.Join(t.TempDir(), "app_2")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &builder.BuildConfig{Language: builder.Rust}
	rep := newNativeBuildReport(builder.Rust, cfg, bin)
	if rep.Cache.Status != buildreport.CacheUnknown {
		t.Fatalf("missing observation must map to unknown, got %q", rep.Cache.Status)
	}
	if rep.Compiler != "rust/cargo" {
		t.Fatalf("rust compiler label wrong: %q", rep.Compiler)
	}
}

func TestEmitBuildReport_RecordingFailureKeepsSuccess(t *testing.T) {
	// Regression analysis is observability: when version recording failed,
	// the report still renders with a warning — and the function never panics
	// nor returns an error that could fail the build.
	var buf bytes.Buffer
	rep := &buildreport.Report{
		Language: "go", Compiler: "go", CompilerVersion: "1.27",
		DurationMS: 1000,
		Cache:      buildreport.CacheInfo{Status: buildreport.CacheHit},
		Artifact:   buildreport.ArtifactInfo{Type: buildreport.ArtifactBinary, SizeBytes: 1024},
	}
	// Swap stdout capture: emitBuildReport writes to os.Stdout; exercise the
	// failure branch through the public API shape only.
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	emitBuildReport("app-x", rep, "", nil, errRecording)
	w.Close()
	os.Stdout = old
	_, _ = buf.ReadFrom(r)
	out := buf.String()
	if !strings.Contains(out, "Build Report") {
		t.Fatalf("report must still render when recording failed:\n%s", out)
	}
	if !strings.Contains(out, "Build report warning") {
		t.Fatalf("expected warning note; got:\n%s", out)
	}
}

var errRecording = os.ErrNotExist

func TestPrintFailedBuildSummary(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	printFailedBuildSummary(builder.Rust, &builder.BuildConfig{
		BuildStartTime: time.Now().Add(-3 * time.Second),
		BuildEndTime:   time.Now(),
		Observe:        &builder.BuildObservation{CacheStatus: buildreport.CacheCold, CompilerVersion: "1.85"},
	})
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	out := buf.String()
	for _, want := range []string{"Failed after", "compile", "COLD", "Rust 1.85"} {
		if !strings.Contains(out, want) {
			t.Errorf("failed-build summary missing %q; got:\n%s", want, out)
		}
	}
}

func matrixResultFor(lang builder.Language, version, platform, cache string) matrix.Result {
	parts := strings.SplitN(platform, "/", 2)
	return matrix.Result{
		Combination: matrix.Combination{
			Lang: lang, Version: version, OS: parts[0], Arch: parts[1], Platform: platform,
		},
		Status:      "success",
		Duration:    21000 * time.Millisecond,
		CacheStatus: cache,
	}
}

func TestMatrixComboReport_IdentityIsolation(t *testing.T) {
	// Per-combination reports must carry the combination's own platform and
	// toolchain so cross-combination comparisons are impossible downstream.
	report := newMatrixComboReport(matrixResultFor(builder.Go, "1.27", "linux/amd64", "cold"))
	if report.IdentityKey() != "go|1.27|linux/amd64|binary" {
		t.Fatalf("identity key = %q", report.IdentityKey())
	}
	other := newMatrixComboReport(matrixResultFor(builder.Go, "1.26", "linux/arm64", "cold"))
	if report.IdentityKey() == other.IdentityKey() {
		t.Fatal("different matrix combinations must not share an identity")
	}
	if report.Cache.Status != buildreport.CacheCold {
		t.Fatalf("cache status propagation broken: %+v", report.Cache)
	}
	unknown := newMatrixComboReport(matrixResultFor(builder.Rust, "1.85", "linux/amd64", ""))
	if unknown.Cache.Status != buildreport.CacheUnknown {
		t.Fatalf("empty cache status must map to unknown, got %q", unknown.Cache.Status)
	}
}
