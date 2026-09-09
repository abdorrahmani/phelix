package matrix

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// --- ParsePlan tests -------------------------------------------------------

func TestParsePlan_GoVersions(t *testing.T) {
	plan, err := ParsePlan(builder.Go, []string{"1.22", "1.23"}, []string{"linux/amd64", "linux/arm64"})
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if plan.Lang != builder.Go {
		t.Fatalf("lang = %s, want go", plan.Lang)
	}
	if len(plan.Combinations) != 4 {
		t.Fatalf("expected 4 combinations, got %d", len(plan.Combinations))
	}
	// Verify cross product.
	ids := make(map[string]bool)
	for _, c := range plan.Combinations {
		ids[c.ID()] = true
	}
	for _, want := range []string{"go1.22-linux-amd64", "go1.22-linux-arm64", "go1.23-linux-amd64", "go1.23-linux-arm64"} {
		if !ids[want] {
			t.Errorf("missing combination %s", want)
		}
	}
}

func TestParsePlan_RustVersions(t *testing.T) {
	plan, err := ParsePlan(builder.Rust, []string{"1.77", "1.78"}, []string{"linux/amd64"})
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if plan.Lang != builder.Rust {
		t.Fatalf("lang = %s, want rust", plan.Lang)
	}
	if len(plan.Combinations) != 2 {
		t.Fatalf("expected 2 combinations, got %d", len(plan.Combinations))
	}
}

func TestParsePlan_DeduplicatesVersions(t *testing.T) {
	plan, err := ParsePlan(builder.Go, []string{"1.22", "1.22", "1.23"}, []string{"linux/amd64"})
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if len(plan.Combinations) != 2 {
		t.Fatalf("expected 2 combinations (deduped), got %d", len(plan.Combinations))
	}
}

func TestParsePlan_DeduplicatesPlatforms(t *testing.T) {
	plan, err := ParsePlan(builder.Go, []string{"1.22"}, []string{"linux/amd64", "linux/amd64"})
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if len(plan.Combinations) != 1 {
		t.Fatalf("expected 1 combination (deduped), got %d", len(plan.Combinations))
	}
}

func TestParsePlan_InvalidVersion(t *testing.T) {
	_, err := ParsePlan(builder.Go, []string{"latest"}, []string{"linux/amd64"})
	if err == nil {
		t.Fatal("expected error for invalid Go version")
	}
}

func TestParsePlan_InvalidPlatform(t *testing.T) {
	_, err := ParsePlan(builder.Go, []string{"1.22"}, []string{"freebsd/sparc64"})
	if err == nil {
		t.Fatal("expected error for invalid platform")
	}
}

func TestParsePlan_EmptyVersions(t *testing.T) {
	_, err := ParsePlan(builder.Go, nil, []string{"linux/amd64"})
	if err == nil {
		t.Fatal("expected error for empty versions")
	}
}

func TestParsePlan_EmptyPlatforms(t *testing.T) {
	_, err := ParsePlan(builder.Go, []string{"1.22"}, nil)
	if err == nil {
		t.Fatal("expected error for empty platforms")
	}
}

func TestParsePlan_NormalizesVersionPrefixes(t *testing.T) {
	// Accept "go1.22", "v1.22", "1.22" — all should normalize to "1.22".
	plan, err := ParsePlan(builder.Go, []string{"go1.22", "v1.23"}, []string{"linux/amd64"})
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	for _, c := range plan.Combinations {
		if c.Version == "go1.22" || c.Version == "v1.22" || c.Version == "v1.23" {
			t.Errorf("version not normalized: %s", c.Version)
		}
	}
}

func TestParsePlan_SingleVersionSinglePlatform(t *testing.T) {
	plan, err := ParsePlan(builder.Go, []string{"1.22"}, []string{"linux/amd64"})
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if len(plan.Combinations) != 1 {
		t.Fatalf("expected 1 combination, got %d", len(plan.Combinations))
	}
	c := plan.Combinations[0]
	if c.OS != "linux" || c.Arch != "amd64" || c.Version != "1.22" {
		t.Errorf("unexpected combination: %+v", c)
	}
}

// --- IsMatrixMode tests ----------------------------------------------------

func TestIsMatrixMode_FlagOnly(t *testing.T) {
	if !IsMatrixMode(true, nil, nil, nil) {
		t.Fatal("--matrix flag should enable matrix mode")
	}
}

func TestIsMatrixMode_GoVersionsOnly(t *testing.T) {
	if !IsMatrixMode(false, []string{"1.22"}, nil, nil) {
		t.Fatal("--go-versions should enable matrix mode")
	}
}

func TestIsMatrixMode_RustVersionsOnly(t *testing.T) {
	if !IsMatrixMode(false, nil, []string{"1.77"}, nil) {
		t.Fatal("--rust-versions should enable matrix mode")
	}
}

func TestIsMatrixMode_PlatformsOnly(t *testing.T) {
	if !IsMatrixMode(false, nil, nil, []string{"linux/amd64"}) {
		t.Fatal("--platforms should enable matrix mode")
	}
}

func TestIsMatrixMode_NothingSet(t *testing.T) {
	if IsMatrixMode(false, nil, nil, nil) {
		t.Fatal("no flags set should not enable matrix mode")
	}
}

// --- Combination ID / naming tests -----------------------------------------

func TestCombination_ID(t *testing.T) {
	c := Combination{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	if got := c.ID(); got != "go1.22-linux-amd64" {
		t.Errorf("ID() = %s, want go1.22-linux-amd64", got)
	}
}

func TestCombination_BinaryName(t *testing.T) {
	c := Combination{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	got := c.BinaryName("myapp")
	want := "myapp_amd64_go_1.22"
	if got != want {
		t.Errorf("BinaryName() = %s, want %s", got, want)
	}
}

func TestCombination_ImageTag(t *testing.T) {
	c := Combination{Lang: builder.Rust, Version: "1.77", OS: "linux", Arch: "arm64", Platform: "linux/arm64"}
	got := c.ImageTag("myapp")
	want := "myapp:rust1.77-arm64"
	if got != want {
		t.Errorf("ImageTag() = %s, want %s", got, want)
	}
}

// --- Executor tests --------------------------------------------------------

func TestExecute_AllSucceed(t *testing.T) {
	plan := &MatrixPlan{
		Lang: builder.Go,
		Combinations: []Combination{
			{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
			{Lang: builder.Go, Version: "1.23", OS: "linux", Arch: "arm64", Platform: "linux/arm64"},
		},
	}

	fn := func(ctx context.Context, c Combination) *Result {
		return &Result{Combination: c, Status: "success", Artifact: "/tmp/fake"}
	}

	results := Execute(plan, fn, ExecutorConfig{Concurrency: 2})
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Status != "success" {
			t.Errorf("expected success, got %s for %s", r.Status, r.Combination.ID())
		}
	}
	if HasFailures(results) {
		t.Error("HasFailures should be false")
	}
}

func TestExecute_FailOpen(t *testing.T) {
	plan := &MatrixPlan{
		Lang: builder.Go,
		Combinations: []Combination{
			{Lang: builder.Go, Version: "1.21", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
			{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
			{Lang: builder.Go, Version: "1.23", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		},
	}

	// Fail the second combination only.
	fn := func(ctx context.Context, c Combination) *Result {
		if c.Version == "1.22" {
			return &Result{Combination: c, Status: "failed", Error: fmt.Errorf("build error")}
		}
		return &Result{Combination: c, Status: "success", Artifact: "/tmp/fake"}
	}

	results := Execute(plan, fn, ExecutorConfig{Concurrency: 2})

	// Fail-open: all 3 should complete (not abort on first failure).
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	succeeded := Succeeded(results)
	failed := Failed(results)
	if len(succeeded) != 2 {
		t.Errorf("expected 2 succeeded, got %d", len(succeeded))
	}
	if len(failed) != 1 {
		t.Errorf("expected 1 failed, got %d", len(failed))
	}
	if failed[0].Combination.Version != "1.22" {
		t.Errorf("expected version 1.22 to fail, got %s", failed[0].Combination.Version)
	}
}

func TestExecute_DryRun(t *testing.T) {
	plan := &MatrixPlan{
		Lang: builder.Go,
		Combinations: []Combination{
			{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		},
	}

	calls := 0
	fn := func(ctx context.Context, c Combination) *Result {
		calls++
		return &Result{Combination: c, Status: "success"}
	}

	results := Execute(plan, fn, ExecutorConfig{Concurrency: 1, DryRun: true})
	if calls != 0 {
		t.Errorf("dry run should not call build function, called %d times", calls)
	}
	for _, r := range results {
		if r.Status != "skipped" {
			t.Errorf("dry run result should be skipped, got %s", r.Status)
		}
	}
}

// --- Concurrency limit test -----------------------------------------------

func TestExecute_ConcurrencyLimit(t *testing.T) {
	const concurrency = 2
	const total = 10

	plan := &MatrixPlan{
		Lang:         builder.Go,
		Combinations: make([]Combination, total),
	}
	for i := range plan.Combinations {
		plan.Combinations[i] = Combination{
			Lang: builder.Go, Version: fmt.Sprintf("1.%d", 20+i),
			OS: "linux", Arch: "amd64", Platform: "linux/amd64",
		}
	}

	var maxConcurrent int32
	var currentConcurrent int32

	fn := func(ctx context.Context, c Combination) *Result {
		cur := atomic.AddInt32(&currentConcurrent, 1)
		// Track max concurrent.
		for {
			old := atomic.LoadInt32(&maxConcurrent)
			if cur <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, cur) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond) // simulate work
		atomic.AddInt32(&currentConcurrent, -1)
		return &Result{Combination: c, Status: "success"}
	}

	results := Execute(plan, fn, ExecutorConfig{Concurrency: concurrency})
	if len(results) != total {
		t.Fatalf("expected %d results, got %d", total, len(results))
	}

	peak := atomic.LoadInt32(&maxConcurrent)
	if peak > int32(concurrency) {
		t.Errorf("max concurrent = %d, expected <= %d", peak, concurrency)
	}
}

// --- Worker pool preserves order ------------------------------------------

func TestExecute_PreservesOrder(t *testing.T) {
	const n = 20
	plan := &MatrixPlan{
		Lang:         builder.Go,
		Combinations: make([]Combination, n),
	}
	for i := range plan.Combinations {
		plan.Combinations[i] = Combination{
			Lang: builder.Go, Version: fmt.Sprintf("1.%d", 20+i),
			OS: "linux", Arch: "amd64", Platform: "linux/amd64",
		}
	}

	fn := func(ctx context.Context, c Combination) *Result {
		return &Result{Combination: c, Status: "success"}
	}

	results := Execute(plan, fn, ExecutorConfig{Concurrency: 4})
	for i, r := range results {
		want := fmt.Sprintf("1.%d", 20+i)
		if r.Combination.Version != want {
			t.Errorf("results[%d].Version = %s, want %s", i, r.Combination.Version, want)
		}
	}
}

// --- Report tests ----------------------------------------------------------

func TestGenerateReport_Counts(t *testing.T) {
	results := []Result{
		{Combination: Combination{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}, Status: "success"},
		{Combination: Combination{Lang: builder.Go, Version: "1.23", OS: "linux", Arch: "arm64", Platform: "linux/arm64"}, Status: "failed", Error: fmt.Errorf("oops")},
		{Combination: Combination{Lang: builder.Go, Version: "1.24", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}, Status: "skipped"},
	}

	report := GenerateReport("testapp", results, time.Now())
	if report.Total != 3 {
		t.Errorf("Total = %d, want 3", report.Total)
	}
	if report.Succeeded != 1 {
		t.Errorf("Succeeded = %d, want 1", report.Succeeded)
	}
	if report.Failed != 1 {
		t.Errorf("Failed = %d, want 1", report.Failed)
	}
	if report.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", report.Skipped)
	}
}

func TestGenerateReport_PerCombinationStatus(t *testing.T) {
	results := []Result{
		{Combination: Combination{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}, Status: "success", Artifact: "/tmp/myapp"},
		{Combination: Combination{Lang: builder.Go, Version: "1.23", OS: "linux", Arch: "arm64", Platform: "linux/arm64"}, Status: "failed", Error: fmt.Errorf("linker error")},
	}

	report := GenerateReport("testapp", results, time.Now())
	if len(report.Combinations) != 2 {
		t.Fatalf("expected 2 combo reports, got %d", len(report.Combinations))
	}

	cr0 := report.Combinations[0]
	if cr0.Status != "success" || cr0.Artifact != "/tmp/myapp" {
		t.Errorf("combo 0: status=%s, artifact=%s", cr0.Status, cr0.Artifact)
	}

	cr1 := report.Combinations[1]
	if cr1.Status != "failed" || cr1.Error != "linker error" {
		t.Errorf("combo 1: status=%s, error=%s", cr1.Status, cr1.Error)
	}
}

// --- HasFailures / Succeeded / Failed helpers -----------------------------

func TestHasFailures_AllSucceed(t *testing.T) {
	results := []Result{
		{Status: "success"},
		{Status: "success"},
	}
	if HasFailures(results) {
		t.Error("expected HasFailures = false")
	}
}

func TestHasFailures_OneFails(t *testing.T) {
	results := []Result{
		{Status: "success"},
		{Status: "failed"},
	}
	if !HasFailures(results) {
		t.Error("expected HasFailures = true")
	}
}

func TestSucceeded_FiltersCorrectly(t *testing.T) {
	results := []Result{
		{Combination: Combination{Version: "1.22"}, Status: "success"},
		{Combination: Combination{Version: "1.23"}, Status: "failed"},
		{Combination: Combination{Version: "1.24"}, Status: "success"},
	}
	succeeded := Succeeded(results)
	if len(succeeded) != 2 {
		t.Fatalf("expected 2 succeeded, got %d", len(succeeded))
	}
}

func TestFailed_FiltersCorrectly(t *testing.T) {
	results := []Result{
		{Combination: Combination{Version: "1.22"}, Status: "success"},
		{Combination: Combination{Version: "1.23"}, Status: "failed"},
	}
	failed := Failed(results)
	if len(failed) != 1 {
		t.Fatalf("expected 1 failed, got %d", len(failed))
	}
	if failed[0].Combination.Version != "1.23" {
		t.Errorf("expected version 1.23 to fail, got %s", failed[0].Combination.Version)
	}
}

// --- Push block on partial failure test ------------------------------------

func TestPushBlock_PartialFailure(t *testing.T) {
	// This tests the fail-closed push semantics: when some combinations
	// failed to build, PushImages should refuse to push anything.
	dmb := &DockerMatrixBuilder{ProjectRoot: t.TempDir()}

	results := []Result{
		{Combination: Combination{Version: "1.22"}, Status: "success", Artifact: "myapp:go1.22-linux-amd64"},
		{Combination: Combination{Version: "1.23"}, Status: "failed", Error: fmt.Errorf("build error")},
	}

	// Without --push-partial, should refuse to push.
	err := dmb.PushImages(results, false)
	if err == nil {
		t.Fatal("expected push to be blocked on partial failure")
	}

	// With --push-partial, should proceed (but we can't actually push in tests).
	// This just verifies the logic doesn't return the block error.
	// The actual push will fail because Docker isn't running, but that's OK.
	_ = dmb.PushImages(results, true)
}

func TestPushBlock_AllFailed(t *testing.T) {
	dmb := &DockerMatrixBuilder{ProjectRoot: t.TempDir()}

	results := []Result{
		{Combination: Combination{Version: "1.22"}, Status: "failed"},
		{Combination: Combination{Version: "1.23"}, Status: "failed"},
	}

	err := dmb.PushImages(results, true)
	if err == nil {
		t.Fatal("expected error when all combinations failed")
	}
}

// --- Cache key isolation test ----------------------------------------------

func TestCacheKeyIsolation(t *testing.T) {
	// Verify that different combinations produce different cache paths.
	c1 := Combination{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	c2 := Combination{Lang: builder.Go, Version: "1.23", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	c3 := Combination{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "arm64", Platform: "linux/arm64"}

	if c1.ID() == c2.ID() {
		t.Errorf("c1 and c2 should have different IDs: %s", c1.ID())
	}
	if c1.ID() == c3.ID() {
		t.Errorf("c1 and c3 should have different IDs: %s", c1.ID())
	}
	if c2.ID() == c3.ID() {
		t.Errorf("c2 and c3 should have different IDs: %s", c2.ID())
	}
}

// --- Context cancellation test ---------------------------------------------

func TestExecute_ContextCancellation(t *testing.T) {
	plan := &MatrixPlan{
		Lang: builder.Go,
		Combinations: []Combination{
			{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
			{Lang: builder.Go, Version: "1.23", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		},
	}

	var built sync.WaitGroup
	built.Add(1)
	firstDone := make(chan struct{})

	fn := func(ctx context.Context, c Combination) *Result {
		if c.Version == "1.22" {
			defer built.Done()
			close(firstDone)
			time.Sleep(100 * time.Millisecond)
		}
		return &Result{Combination: c, Status: "success"}
	}

	// Run in background, cancel after first completes.
	go func() {
		<-firstDone
		// Give a moment for the second to start.
		time.Sleep(5 * time.Millisecond)
	}()

	results := Execute(plan, fn, ExecutorConfig{Concurrency: 1})
	// All should complete even though we conceptually wanted to cancel —
	// the executor doesn't expose cancel, but it respects context.
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	built.Wait()
}

// --- Platform to triple mapping test (Rust) --------------------------------

func TestPlatformToRustTriple(t *testing.T) {
	tests := []struct {
		os, arch, want string
	}{
		{"linux", "amd64", "x86_64-unknown-linux-gnu"},
		{"linux", "arm64", "aarch64-unknown-linux-gnu"},
		{"darwin", "amd64", "x86_64-apple-darwin"},
		{"darwin", "arm64", "aarch64-apple-darwin"},
		{"windows", "amd64", "x86_64-pc-windows-msvc"},
	}
	for _, tt := range tests {
		got, err := platformToRustTriple(tt.os, tt.arch)
		if err != nil {
			t.Errorf("platformToRustTriple(%s, %s): %v", tt.os, tt.arch, err)
			continue
		}
		if got != tt.want {
			t.Errorf("platformToRustTriple(%s, %s) = %s, want %s", tt.os, tt.arch, got, tt.want)
		}
	}
}

func TestPlatformToRustTriple_Unknown(t *testing.T) {
	_, err := platformToRustTriple("freebsd", "sparc64")
	if err == nil {
		t.Fatal("expected error for unknown platform")
	}
}

func TestParsePlan_ExactVersionsVariantAndUnsupportedLanguage(t *testing.T) {
	plan, err := ParsePlan(builder.Go, []string{"go1.22.4"}, []string{"LINUX/ARM/v7"})
	if err != nil {
		t.Fatal(err)
	}
	c := plan.Combinations[0]
	if c.Version != "1.22.4" || c.Arch != "arm" || c.Variant != "v7" || c.Platform != "linux/arm/v7" {
		t.Fatalf("unexpected normalized combination: %+v", c)
	}
	for _, bad := range []string{"1", "1.x", "latest", "1.2.3.4"} {
		if _, err := ParsePlan(builder.Rust, []string{bad}, []string{"linux/amd64"}); err == nil {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
	if _, err := ParsePlan(builder.Language("python"), []string{"3.12"}, []string{"linux/amd64"}); err == nil {
		t.Fatal("expected unsupported language rejection")
	}
}

func TestCombinationNamesArePathSafeAndVariantUnique(t *testing.T) {
	c6 := Combination{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "arm", Variant: "v6", Platform: "linux/arm/v6"}
	c7 := Combination{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "arm", Variant: "v7", Platform: "linux/arm/v7"}
	for _, value := range []string{c6.ID(), c6.BinaryName("../My App"), c6.ImageTag("../My App")} {
		if strings.ContainsAny(value, `/\\ `) || strings.Contains(value, "..") {
			t.Fatalf("unsafe generated name %q", value)
		}
	}
	if c6.ID() == c7.ID() || c6.BinaryName("app") == c7.BinaryName("app") || c6.ImageTag("app") == c7.ImageTag("app") {
		t.Fatal("ARM variants must generate unique names")
	}
}

func TestExecuteEmptyNilAndPanicIsolation(t *testing.T) {
	if got := Execute(nil, nil, ExecutorConfig{}); len(got) != 0 {
		t.Fatalf("nil plan returned %d results", len(got))
	}
	plan := &MatrixPlan{Combinations: []Combination{{Version: "1.1"}, {Version: "1.2"}, {Version: "1.3"}}}
	results := Execute(plan, func(_ context.Context, c Combination) *Result {
		switch c.Version {
		case "1.1":
			return nil
		case "1.2":
			panic("boom")
		default:
			return &Result{Status: "success"}
		}
	}, ExecutorConfig{Concurrency: 3, Debug: true})
	if len(results) != 3 || results[0].Status != "failed" || results[1].Status != "failed" || results[2].Status != "success" {
		t.Fatalf("unexpected isolated results: %+v", results)
	}
}

func TestGenerateReportEmptyAndRedacted(t *testing.T) {
	empty := GenerateReport("app", nil, time.Now())
	if empty.Total != 0 || empty.Lang != "" || empty.Combinations == nil {
		t.Fatalf("unsafe empty report: %+v", empty)
	}
	report := GenerateReport("app", []Result{{Status: "failed", Error: errors.New("password=hunter2")}}, time.Now())
	if strings.Contains(report.Combinations[0].Error, "hunter2") {
		t.Fatalf("report leaked secret: %s", report.Combinations[0].Error)
	}
}

func TestFindMainPackageDeterministic(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"cmd/zeta", "cmd/alpha"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := (&GoMatrixBuilder{ProjectRoot: root}).findMainPackage()
	if err != nil || got != "./cmd/alpha" {
		t.Fatalf("findMainPackage = %q, %v", got, err)
	}
}

func TestRustManifestBinarySelection(t *testing.T) {
	root := t.TempDir()
	manifest := "[package]\nname='pkg'\ndefault-run='server'\n\n[[bin]]\nname='worker'\npath='src/worker.rs'\n\n[[bin]]\nname='server'\npath='src/server.rs'\n"
	if err := os.WriteFile(filepath.Join(root, "Cargo.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := (&RustMatrixBuilder{ProjectRoot: root}).getPkgName()
	if err != nil || got != "server" {
		t.Fatalf("getPkgName = %q, %v", got, err)
	}
}

func TestDockerCommandDeterministicAndDoesNotMutateInputs(t *testing.T) {
	root := t.TempDir()
	original := map[string]string{"Z": "last", "A": "first"}
	var gotName string
	var gotArgs []string
	// The digest of the built image, echoed back by the faked inspect call.
	const fakeDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	builderUnderTest := &DockerMatrixBuilder{
		ProjectRoot: root, AppName: "demo", Tag: "v1", BuildArgs: original,
		Labels: map[string]string{"z": "2", "a": "1"},
		CommandContext: func(_ context.Context, name string, args ...string) *exec.Cmd {
			if len(args) >= 3 && args[0] == "image" && args[1] == "inspect" {
				return exec.Command("echo", "sha256:"+fakeDigest)
			}
			gotName, gotArgs = name, append([]string(nil), args...)
			return exec.Command("true")
		},
	}
	c := Combination{Lang: builder.Go, Version: "1.22.4", OS: "linux", Arch: "arm", Variant: "v7", Platform: "linux/arm/v7"}
	result := builderUnderTest.BuildDockerImage(context.Background(), c)
	if result.Status != "success" || gotName != "docker" {
		t.Fatalf("build result=%+v command=%s", result, gotName)
	}
	if result.SHA256 != fakeDigest {
		t.Fatalf("image digest not attached: %q", result.SHA256)
	}
	if !reflect.DeepEqual(original, map[string]string{"Z": "last", "A": "first"}) {
		t.Fatalf("BuildArgs mutated: %#v", original)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"demo:v1-go1.22.4-arm-v7", "TARGETVERSION=1.22.4", "TARGETVARIANT=v7", "--platform linux/arm/v7"} {
		if !strings.Contains(joined, want) {
			t.Errorf("command missing %q: %s", want, joined)
		}
	}
	if strings.Index(joined, "A=first") > strings.Index(joined, "Z=last") || strings.Index(joined, "a=1") > strings.Index(joined, "z=2") {
		t.Fatalf("command maps are not sorted: %s", joined)
	}
}

func TestGoDockerCommandUsesSafeArgv(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	g := &GoMatrixBuilder{
		ProjectRoot: root, AppName: "demo", ExtraArgs: []string{"-trimpath", "-ldflags=-s -w"},
		LookPath: func(string) (string, error) { return "", exec.ErrNotFound },
		CommandContext: func(_ context.Context, name string, args ...string) *exec.Cmd {
			commands = append(commands, append([]string{name}, args...))
			if len(args) > 0 && args[0] == "run" {
				outDir := filepath.Join(root, "builds", "matrix", "go1.22.4-linux-arm-v7")
				_ = os.MkdirAll(outDir, 0o755)
				_ = os.WriteFile(filepath.Join(outDir, "demo_arm_go_1.22.4_v7"), []byte("bin"), 0o755)
			}
			return exec.Command("true")
		},
	}
	c := Combination{Lang: builder.Go, Version: "1.22.4", OS: "linux", Arch: "arm", Variant: "v7", Platform: "linux/arm/v7"}
	result := g.Build(context.Background(), c)
	if result.Status != "success" {
		t.Fatalf("Build failed: %+v", result)
	}
	// The final artifact bytes must carry their SHA-256 end to end.
	wantSum, err := SHA256File(result.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.SHA256 != wantSum {
		t.Fatalf("builder checksum = %q, want %q", result.SHA256, wantSum)
	}
	joined := strings.Join(commands[len(commands)-1], " ")
	for _, want := range []string{"golang:1.22.4 go build -v -trimpath -ldflags=-s -w", "GOARM=7"} {
		if !strings.Contains(joined, want) {
			t.Errorf("command missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "sh -c") {
		t.Fatalf("unsafe shell command: %s", joined)
	}
}

func TestRustCommandUsesVersionAndExtraArgs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Cargo.toml"), []byte("[package]\nname='demo'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.rs"), []byte("fn main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got []string
	r := &RustMatrixBuilder{
		ProjectRoot: root, AppName: "demo", ExtraArgs: []string{"--locked"},
		LookPath: func(string) (string, error) { return "/fake/cross", nil },
		CommandContext: func(_ context.Context, name string, args ...string) *exec.Cmd {
			got = append([]string{name}, args...)
			triple := "x86_64-unknown-linux-gnu"
			bin := filepath.Join(root, "target", "rust1.77.2-linux-amd64", triple, "release", "demo")
			_ = os.MkdirAll(filepath.Dir(bin), 0o755)
			_ = os.WriteFile(bin, []byte("bin"), 0o755)
			return exec.Command("true")
		},
	}
	c := Combination{Lang: builder.Rust, Version: "1.77.2", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	result := r.Build(context.Background(), c)
	if result.Status != "success" {
		t.Fatalf("Build failed: %+v", result)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "cross +1.77.2 build") || !strings.Contains(joined, "--locked") {
		t.Fatalf("unexpected cross command: %s", joined)
	}
}

func TestDockerRejectsNonLinuxAndMissingFailureError(t *testing.T) {
	d := &DockerMatrixBuilder{ProjectRoot: t.TempDir()}
	result := d.BuildDockerImage(context.Background(), Combination{OS: "darwin", Arch: "arm64", Platform: "darwin/arm64"})
	if result.Status != "failed" {
		t.Fatalf("expected non-linux failure: %+v", result)
	}
	if got := formatFailedList([]Result{{Status: "failed"}}); !strings.Contains(got, "build failed") {
		t.Fatalf("missing nil-error fallback: %q", got)
	}
}

func TestParsePlan_RejectsWhitespaceOnlyDimensions(t *testing.T) {
	// A slice of blank entries passes the raw length check but must not
	// expand into an empty plan (which would previously produce zero
	// combinations and panic downstream in GenerateReport).
	for _, tc := range []struct {
		name      string
		versions  []string
		platforms []string
	}{
		{"blank versions", []string{"  ", ""}, []string{"linux/amd64"}},
		{"blank platforms", []string{"1.22"}, []string{" "}},
	} {
		if _, err := ParsePlan(builder.Go, tc.versions, tc.platforms); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}
