package deployanalysis

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func useTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func recordResourceSeries(t *testing.T, app string, start time.Time, count int, cpu, memory float64) {
	t.Helper()
	for i := 0; i < count; i++ {
		RecordAppMetric(app, start.Add(time.Duration(i)*10*time.Second), cpu, uint64(memory))
	}
}

func recordTrafficSeries(t *testing.T, app string, start time.Time, count int, reqPerSecond int64, errorEvery int64, p95 time.Duration) {
	t.Helper()
	for i := 0; i < count; i++ {
		requests := int64(i) * reqPerSecond * 10
		var errs int64
		if errorEvery > 0 {
			errs = requests / errorEvery
		}
		RecordTraffic(app, start.Add(time.Duration(i)*10*time.Second), requests, errs, p95)
	}
}

func aggregatePair(cpuBefore, cpuAfter, memBefore, memAfter, errBefore, errAfter, p95Before, p95After, rpsBefore, rpsAfter float64) (WindowAggregate, WindowAggregate) {
	now := time.Now().UTC()
	before := WindowAggregate{
		StartedAt: now.Add(-5 * time.Minute), EndedAt: now.Add(-2 * time.Minute),
		ResourceSamples: 10, TrafficSamples: 10,
		HasCPU: true, CPUAvg: cpuBefore, CPUPeak: cpuBefore,
		HasMemory: true, MemoryAvg: memBefore, MemoryPeak: memBefore,
		HasTraffic: true, Requests: 1000, Errors: int64(errBefore * 10),
		ErrorRate: errBefore, P95MS: p95Before, Throughput: rpsBefore,
	}
	after := WindowAggregate{
		StartedAt: now.Add(-2 * time.Minute), EndedAt: now,
		ResourceSamples: 10, TrafficSamples: 10,
		HasCPU: true, CPUAvg: cpuAfter, CPUPeak: cpuAfter,
		HasMemory: true, MemoryAvg: memAfter, MemoryPeak: memAfter,
		HasTraffic: true, Requests: 1000, Errors: int64(errAfter * 10),
		ErrorRate: errAfter, P95MS: p95After, Throughput: rpsAfter,
	}
	return before, after
}

func TestAnalyzeNormalDeployment(t *testing.T) {
	before, after := aggregatePair(10, 10, 100<<20, 100<<20, 1, 1, 20, 20, 100, 100)
	rep := Analyze(ReportInput{
		AnalysisID:      "a1",
		DeploymentID:    "d1",
		AppName:         "demo",
		Strategy:        "rolling",
		PreviousVersion: "v1",
		NewVersion:      "v2",
		DeployedAt:      time.Now(),
		CompletedAt:     time.Now(),
	}, before, after, DefaultConfig())

	if rep.Status != StatusNormal {
		t.Fatalf("status = %q, want normal (metrics %+v)", rep.Status, rep.Metrics)
	}
	if rep.Outcome != OutcomePromoted {
		t.Fatalf("outcome = %q, want promoted", rep.Outcome)
	}
	if rep.ImpactScore != 100 {
		t.Fatalf("impact score = %v, want 100", rep.ImpactScore)
	}
	if rep.Metric(MetricErrorRate).Before == nil || rep.Metric(MetricErrorRate).After == nil {
		t.Fatalf("expected traffic metrics to be measured: %+v", rep.Metric(MetricErrorRate))
	}
}

func TestAnalyzeRegressionAndScoring(t *testing.T) {
	before, after := aggregatePair(10, 50, 100<<20, 200<<20, 0.1, 10, 20, 80, 100, 100)
	rep := Analyze(ReportInput{
		AnalysisID: "a2", DeploymentID: "d2", AppName: "regress", Strategy: "canary",
		PreviousVersion: "v1", NewVersion: "v2", DeployedAt: time.Now(), CompletedAt: time.Now(),
	}, before, after, DefaultConfig())

	if rep.Status != StatusRegression {
		t.Fatalf("status = %q, want regression", rep.Status)
	}
	if rep.Outcome != OutcomePromotedWithRegressions {
		t.Fatalf("outcome = %q, want promoted_with_regressions", rep.Outcome)
	}
	if rep.ImpactScore >= 100 {
		t.Fatalf("impact score = %v, want a penalty", rep.ImpactScore)
	}
	if rep.Metric(MetricErrorRate).Status != StatusRegression {
		t.Fatalf("error rate status = %q", rep.Metric(MetricErrorRate).Status)
	}
	if rep.Metric(MetricP95Latency).Status != StatusRegression {
		t.Fatalf("latency status = %q", rep.Metric(MetricP95Latency).Status)
	}
	if rep.ImpactGrade == "A" {
		t.Fatalf("grade = %q, want a lower grade", rep.ImpactGrade)
	}
}

func TestAnalyzeThroughputDegradation(t *testing.T) {
	before, after := aggregatePair(10, 10, 100<<20, 100<<20, 1, 1, 20, 20, 100, 50)
	rep := Analyze(ReportInput{AnalysisID: "a4", AppName: "slow"}, before, after, DefaultConfig())
	if rep.Metric(MetricThroughput).Status != StatusRegression {
		t.Fatalf("throughput = %q, want regression", rep.Metric(MetricThroughput).Status)
	}
}

func TestAnalyzeMissingAndInsufficientData(t *testing.T) {
	useTempHome(t)
	app := "sparse"
	now := time.Now().UTC()
	RecordAppMetric(app, now.Add(-2*time.Minute), 10, 100<<20)

	before, _ := AggregateWindow(app, now.Add(-5*time.Minute), now.Add(-3*time.Minute))
	after, _ := AggregateWindow(app, now.Add(-3*time.Minute), now)
	rep := Analyze(ReportInput{AnalysisID: "a3", AppName: app}, before, after, DefaultConfig())

	if rep.Status != StatusInsufficientData {
		t.Fatalf("status = %q, want insufficient_data", rep.Status)
	}
	if rep.Metric(MetricErrorRate).Status != StatusNotAvailable {
		t.Fatalf("error rate = %q, want not_available", rep.Metric(MetricErrorRate).Status)
	}
	if rep.Metric(MetricCPUUsage).Status != StatusInsufficientData {
		t.Fatalf("cpu = %q, want insufficient_data", rep.Metric(MetricCPUUsage).Status)
	}
}

func TestStoreRoundTripAndPruning(t *testing.T) {
	useTempHome(t)
	app := "store"
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		rep := &Report{
			SchemaVersion: SchemaVersion,
			AnalysisID:    NewAnalysisID(app, "d", "v2", now),
			AppName:       app,
			Status:        StatusNormal,
			AnalyzedAt:    now.Add(time.Duration(i) * time.Second),
		}
		rep.AnalysisID = rep.AnalysisID + string(rune('a'+i))
		if err := SaveReport(app, rep); err != nil {
			t.Fatal(err)
		}
	}
	reports, err := ListReports(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 3 {
		t.Fatalf("len = %d, want 3", len(reports))
	}
	if reports[0].AnalyzedAt.Before(reports[2].AnalyzedAt) {
		t.Fatal("reports are not newest first")
	}

	rep := &Report{AnalysisID: "replace", AppName: app, Status: StatusRegression, AnalyzedAt: now.Add(time.Hour)}
	if err := SaveReport(app, rep); err != nil {
		t.Fatal(err)
	}
	rep.Status = StatusImprovement
	if err := SaveReport(app, rep); err != nil {
		t.Fatal(err)
	}
	got, err := GetReport(app, "replace")
	if err != nil || got == nil {
		t.Fatalf("get = %v, %v", got, err)
	}
	if got.Status != StatusImprovement {
		t.Fatalf("status = %q, want the replaced value", got.Status)
	}
}

func TestStoreCorruptionIsQuarantined(t *testing.T) {
	useTempHome(t)
	app := "broken"
	path, err := reportsPath(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	reports, err := ListReports(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 0 {
		t.Fatalf("expected no reports, got %d", len(reports))
	}
	matches, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(matches) == 0 {
		t.Fatalf("corrupt file not quarantined: %v %v", matches, err)
	}
	if err := SaveReport(app, &Report{AnalysisID: "new", AppName: app, AnalyzedAt: time.Now()}); err != nil {
		t.Fatalf("save after corruption: %v", err)
	}
}

func TestPendingFinalizesIntoReport(t *testing.T) {
	useTempHome(t)
	app := "pending"
	now := time.Now().UTC()
	input := ReportInput{
		AnalysisID: "p1", DeploymentID: "d1", AppName: app, Strategy: "rolling",
		NewVersion: "v2", DeployedAt: now.Add(-10 * time.Minute), CompletedAt: now.Add(-9 * time.Minute),
	}
	recordTrafficSeries(t, app, now.Add(-9*time.Minute), 12, 100, 100, 20*time.Millisecond)
	if err := updatePending(app, func([]PendingReport) []PendingReport {
		return []PendingReport{{Input: input, ObserveUntil: now.Add(-time.Second)}}
	}); err != nil {
		t.Fatal(err)
	}
	created, err := FinalizeDue(app, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 {
		t.Fatalf("created = %d, want 1", len(created))
	}
	count, err := PendingCount(app)
	if err != nil || count != 0 {
		t.Fatalf("pending = %d, %v; want 0", count, err)
	}
	reports, err := ListReports(app)
	if err != nil || len(reports) != 1 {
		t.Fatalf("reports = %d, %v; want 1", len(reports), err)
	}
	if reports[0].AnalysisID != "p1" {
		t.Fatalf("analysis id = %q", reports[0].AnalysisID)
	}
}
