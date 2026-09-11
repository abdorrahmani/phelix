package deploy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// verifierHarness wires a Rollout whose default verifier runs against live
// httptest backends and a scripted stats client.
type verifierHarness struct {
	ro        *Rollout
	canarySrv *httptest.Server
	stableSrv *httptest.Server
	pc        *rolloutProxyClient
	log       *fakeLogger
}

func newVerifierHarness(t *testing.T) *verifierHarness {
	t.Helper()
	canary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "canary")
	}))
	t.Cleanup(canary.Close)
	stable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "stable")
	}))
	t.Cleanup(stable.Close)

	log := &fakeLogger{}
	pc := &rolloutProxyClient{alive: true, enrolled: true}
	ro := &Rollout{
		AppName: "verify-app", AppID: "id-1", PublicPort: 8090,
		Plan: RolloutPlan{
			Strategy:     StrategyProgressive,
			Steps:        []RolloutStep{{TrafficPercent: 5}, {TrafficPercent: 100}},
			Verification: VerificationConfig{Interval: 5 * time.Millisecond},
		},
		ProxyClient: pc,
		Logger:      log,
	}
	return &verifierHarness{ro: ro, canarySrv: canary, stableSrv: stable, pc: pc, log: log}
}

func (h *verifierHarness) port(srv *httptest.Server) int {
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

func (h *verifierHarness) stepContext() StepContext {
	return StepContext{
		Step: 0, TotalSteps: 2, TrafficPercent: 5, Duration: 0,
		CanaryPort: h.port(h.canarySrv), CanaryPID: 0, CanaryVersion: 13,
		StablePort: h.port(h.stableSrv), StablePID: 0, StableVersion: 12,
		Tier:    health.Tier2HTTPAny,
		TierCfg: fastCfg(),
	}
}

// fullBuckets builds a cumulative latency histogram where all count requests
// land within the bucket at index idx.
func fullBuckets(count uint64, idx int) []uint64 {
	buckets := make([]uint64, len(proxy.LatencyBucketBounds))
	for i := idx; i < len(buckets); i++ {
		buckets[i] = count
	}
	return buckets
}

// TestVerifier_HealthyCanaryPasses is the green path: healthy backends, sane
// metrics, no regression.
func TestVerifier_HealthyCanaryPasses(t *testing.T) {
	h := newVerifierHarness(t)
	h.pc.stats = [][]proxy.BackendStat{
		// window start
		{
			{Host: hostPort(h.port(h.canarySrv)), Requests: 0, Errors: 0, Buckets: fullBuckets(0, 0)},
			{Host: hostPort(h.port(h.stableSrv)), Requests: 500, Errors: 1, Buckets: fullBuckets(500, 1)},
		},
		// window end: canary 100 requests / 0 errors, stable doubled
		{
			{Host: hostPort(h.port(h.canarySrv)), Requests: 100, Errors: 0, Buckets: fullBuckets(100, 1)},
			{Host: hostPort(h.port(h.stableSrv)), Requests: 1000, Errors: 2, Buckets: fullBuckets(1000, 1)},
		},
	}
	if err := h.ro.verifyStep(context.Background(), h.stepContext()); err != nil {
		t.Fatalf("verifyStep: %v", err)
	}
}

// TestVerifier_StatsUnavailableFallsBackToHealthOnly: when the daemon cannot
// serve per-backend metrics the rollout still verifies on health alone.
func TestVerifier_StatsUnavailableFallsBackToHealthOnly(t *testing.T) {
	h := newVerifierHarness(t)
	h.pc.statsErr = errors.New("unknown op: stats")
	if err := h.ro.verifyStep(context.Background(), h.stepContext()); err != nil {
		t.Fatalf("verifyStep: %v", err)
	}
	found := false
	for _, line := range h.log.lines {
		if strings.Contains(line, "health-only verification") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a health-only fallback note in the log, got %v", h.log.lines)
	}
}

// TestVerifier_CanaryHealthFailure: a canary that stops answering its health
// probe during the window is a regression, not a pass.
func TestVerifier_CanaryHealthFailure(t *testing.T) {
	h := newVerifierHarness(t)
	h.canarySrv.Close()
	err := h.ro.verifyStep(context.Background(), h.stepContext())
	if err == nil || phelixerr.CodeOf(err) != phelixerr.CodeCanaryRegression {
		t.Fatalf("err = %v, want CANARY_REGRESSION", err)
	}
	if !strings.Contains(err.Error(), "failed its health check") {
		t.Errorf("error %q should name the health check", err.Error())
	}
}

// TestVerifier_CanaryProcessDeath: a canary process that exits mid-window is
// reported as a regression.
func TestVerifier_CanaryProcessDeath(t *testing.T) {
	h := newVerifierHarness(t)
	sc := h.stepContext()
	sc.CanaryPID = 999999999 // not running
	err := h.ro.verifyStep(context.Background(), sc)
	if err == nil || phelixerr.CodeOf(err) != phelixerr.CodeCanaryRegression {
		t.Fatalf("err = %v, want CANARY_REGRESSION", err)
	}
	if !strings.Contains(err.Error(), "exited while serving") {
		t.Errorf("error %q should name the process exit", err.Error())
	}
}

// TestVerifier_StableBaselineDeath: a dying STABLE backend fails verification
// with the sentinel the engine uses to keep the canary alive.
func TestVerifier_StableBaselineDeath(t *testing.T) {
	h := newVerifierHarness(t)
	h.stableSrv.Close()
	err := h.ro.verifyStep(context.Background(), h.stepContext())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrStableBaselineUnhealthy) {
		t.Fatalf("err = %v, want ErrStableBaselineUnhealthy", err)
	}
}

// TestVerifier_ErrorRateRegression: the headline canary failure — the canary's
// error rate explodes relative to the baseline.
func TestVerifier_ErrorRateRegression(t *testing.T) {
	h := newVerifierHarness(t)
	h.pc.stats = [][]proxy.BackendStat{
		{ // start
			{Host: hostPort(h.port(h.canarySrv)), Requests: 0, Errors: 0, Buckets: fullBuckets(0, 0)},
			{Host: hostPort(h.port(h.stableSrv)), Requests: 1000, Errors: 3, Buckets: fullBuckets(1000, 1)},
		},
		{ // end: canary 4.82% vs stable 0.31%
			{Host: hostPort(h.port(h.canarySrv)), Requests: 124, Errors: 6, Buckets: fullBuckets(124, 1)},
			{Host: hostPort(h.port(h.stableSrv)), Requests: 2000, Errors: 6, Buckets: fullBuckets(2000, 1)},
		},
	}
	err := h.ro.verifyStep(context.Background(), h.stepContext())
	if err == nil || phelixerr.CodeOf(err) != phelixerr.CodeCanaryRegression {
		t.Fatalf("err = %v, want CANARY_REGRESSION", err)
	}
	msg := err.Error()
	for _, want := range []string{"error rate", "baseline 0.30%", "canary 4.84%"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

// TestVerifier_AbsoluteErrorRateCap: even with a perfect baseline, a canary
// above the absolute error-rate cap regresses.
func TestVerifier_AbsoluteErrorRateCap(t *testing.T) {
	h := newVerifierHarness(t)
	// Cap the delta generously so only the absolute cap can fire.
	h.ro.Plan.Verification.MaxErrorDelta = 100
	h.pc.stats = [][]proxy.BackendStat{
		{
			{Host: hostPort(h.port(h.canarySrv)), Requests: 0, Errors: 0},
			{Host: hostPort(h.port(h.stableSrv)), Requests: 0, Errors: 0},
		},
		{
			{Host: hostPort(h.port(h.canarySrv)), Requests: 100, Errors: 8}, // 8% > 5% default cap
			{Host: hostPort(h.port(h.stableSrv)), Requests: 100, Errors: 0},
		},
	}
	err := h.ro.verifyStep(context.Background(), h.stepContext())
	if err == nil || phelixerr.CodeOf(err) != phelixerr.CodeCanaryRegression {
		t.Fatalf("err = %v, want CANARY_REGRESSION", err)
	}
	if !strings.Contains(err.Error(), "configured maximum") {
		t.Errorf("error %q should name the absolute cap", err.Error())
	}
}

// TestVerifier_LatencyRegression: a canary whose p95 latency blows past the
// configured factor over the baseline regresses.
func TestVerifier_LatencyRegression(t *testing.T) {
	h := newVerifierHarness(t)
	h.pc.stats = [][]proxy.BackendStat{
		{
			{Host: hostPort(h.port(h.canarySrv)), Requests: 0, Errors: 0, Buckets: fullBuckets(0, 0)},
			{Host: hostPort(h.port(h.stableSrv)), Requests: 0, Errors: 0, Buckets: fullBuckets(0, 0)},
		},
		{
			// canary: all 100 requests in the 500ms bucket; stable: all in 5ms.
			{Host: hostPort(h.port(h.canarySrv)), Requests: 100, Errors: 0, Buckets: fullBuckets(100, 6)},
			{Host: hostPort(h.port(h.stableSrv)), Requests: 100, Errors: 0, Buckets: fullBuckets(100, 0)},
		},
	}
	err := h.ro.verifyStep(context.Background(), h.stepContext())
	if err == nil || phelixerr.CodeOf(err) != phelixerr.CodeCanaryRegression {
		t.Fatalf("err = %v, want CANARY_REGRESSION", err)
	}
	if !strings.Contains(err.Error(), "p95 latency") {
		t.Errorf("error %q should name the latency regression", err.Error())
	}
}

// TestVerifier_NoCanaryTrafficSkipsComparison: a canary that served nothing
// during the window (idle app) cannot be compared and passes on health.
func TestVerifier_NoCanaryTrafficSkipsComparison(t *testing.T) {
	h := newVerifierHarness(t)
	h.pc.stats = [][]proxy.BackendStat{
		{{Host: hostPort(h.port(h.canarySrv))}, {Host: hostPort(h.port(h.stableSrv))}},
		{{Host: hostPort(h.port(h.canarySrv)), Requests: 0}, {Host: hostPort(h.port(h.stableSrv)), Requests: 50, Errors: 0}},
	}
	if err := h.ro.verifyStep(context.Background(), h.stepContext()); err != nil {
		t.Fatalf("verifyStep: %v", err)
	}
	found := false
	for _, line := range h.log.lines {
		if strings.Contains(line, "no canary traffic observed") {
			found = true
		}
	}
	if !found {
		t.Error("expected the skipped-comparison note in the log")
	}
}

// TestVerifier_CanaryAbsentFromStartSnapshotKeepsHistogram: a backend with no
// traffic at window start (absent from the start snapshot) still reports its
// latency histogram for the window instead of degrading p95 to n/a.
func TestVerifier_CanaryAbsentFromStartSnapshotKeepsHistogram(t *testing.T) {
	h := newVerifierHarness(t)
	h.pc.stats = [][]proxy.BackendStat{
		{ // start: only the stable backend has recorded anything
			{Host: hostPort(h.port(h.stableSrv)), Requests: 10, Errors: 0, Buckets: fullBuckets(10, 1)},
		},
		{ // end: canary served 100 fast requests since
			{Host: hostPort(h.port(h.canarySrv)), Requests: 100, Errors: 0, Buckets: fullBuckets(100, 1)},
			{Host: hostPort(h.port(h.stableSrv)), Requests: 110, Errors: 0, Buckets: fullBuckets(110, 1)},
		},
	}
	if err := h.ro.verifyStep(context.Background(), h.stepContext()); err != nil {
		t.Fatalf("verifyStep: %v", err)
	}
	found := false
	for _, line := range h.log.lines {
		// The canary line must carry a concrete p95, not "n/a".
		if strings.Contains(line, "v13:") && strings.Contains(line, "p95 ") && !strings.Contains(line, "n/a") {
			found = true
		}
	}
	if !found {
		t.Errorf("canary summary should include a p95 estimate, got %v", h.log.lines)
	}
}

// TestVerifier_WindowPollsUntilFailure: with a nonzero window the verifier
// keeps probing and fails as soon as the canary dies mid-window.
func TestVerifier_WindowPollsUntilFailure(t *testing.T) {
	h := newVerifierHarness(t)
	sc := h.stepContext()
	sc.Duration = 60 * time.Millisecond

	go func() {
		time.Sleep(20 * time.Millisecond)
		h.canarySrv.Close()
	}()
	start := time.Now()
	err := h.ro.verifyStep(context.Background(), sc)
	elapsed := time.Since(start)
	if err == nil || phelixerr.CodeOf(err) != phelixerr.CodeCanaryRegression {
		t.Fatalf("err = %v, want CANARY_REGRESSION from the mid-window probe", err)
	}
	// The failure surfaced promptly (the probe cadence is 5ms), long before
	// the window elapsed.
	if elapsed > 55*time.Millisecond {
		t.Errorf("failure took %s; the mid-window probe should fail fast", elapsed)
	}
}

// TestVerifier_WindowCompletesHealthy: a nonzero window with a healthy canary
// passes after observing for the whole duration.
func TestVerifier_WindowCompletesHealthy(t *testing.T) {
	h := newVerifierHarness(t)
	sc := h.stepContext()
	sc.Duration = 25 * time.Millisecond
	h.ro.Plan.Verification.Interval = 10 * time.Millisecond
	if err := h.ro.verifyStep(context.Background(), sc); err != nil {
		t.Fatalf("verifyStep: %v", err)
	}
}

// TestVerifier_EngineAbortsOnMetricRegression runs the default verifier inside
// a full engine rollout: the scripted metrics produce a regression, the engine
// aborts and restores the stable version to 100% of traffic.
func TestVerifier_EngineAbortsOnMetricRegression(t *testing.T) {
	resetHome(t)
	app := "metric-regress-app"
	stable := seedStableBlueGreen(t, app, 12)

	pc := &rolloutProxyClient{alive: true, enrolled: true}
	ro := rolloutForTest(t, app, []RolloutStep{{TrafficPercent: 5, Duration: 0}, {TrafficPercent: 100}}, pc, versionedSource{version: 13})
	// The engine passes the live canary/stable ports into the verifier, so
	// the scripted stats are keyed to whatever ports this rollout uses.
	ro.Verify = func(ctx context.Context, sc StepContext) error {
		statsPC := &rolloutProxyClient{alive: true, enrolled: true}
		statsPC.stats = [][]proxy.BackendStat{
			{
				{Host: hostPort(sc.CanaryPort), Requests: 0, Errors: 0},
				{Host: hostPort(sc.StablePort), Requests: 0, Errors: 0},
			},
			{
				{Host: hostPort(sc.CanaryPort), Requests: 100, Errors: 10}, // 10%
				{Host: hostPort(sc.StablePort), Requests: 100, Errors: 0},  // 0%
			},
		}
		inner := *ro
		inner.ProxyClient = statsPC
		return inner.verifyStep(ctx, sc)
	}

	err := ro.Deploy(context.Background())
	if err == nil || phelixerr.CodeOf(err) != phelixerr.CodeCanaryRegression {
		t.Fatalf("err = %v, want CANARY_REGRESSION", err)
	}
	restore := pc.lastSwitchSet()
	if len(restore) != 1 || restore[0].Host != hostPort(stable.Port) {
		t.Errorf("last membership = %+v, want the stable slot alone", restore)
	}
	state := mustLoad(t, app)
	if state.Canary == nil || state.Canary.Status != CanaryFailed {
		t.Errorf("canary record = %+v, want failed", state.Canary)
	}
}

// TestQuantileFromBuckets unit-tests the histogram quantile estimation the
// latency comparison depends on.
func TestQuantileFromBuckets(t *testing.T) {
	if got := quantileFromBuckets(nil, 0.95); got != 0 {
		t.Errorf("nil buckets -> %v, want 0", got)
	}
	if got := quantileFromBuckets(fullBuckets(0, 0), 0.95); got != 0 {
		t.Errorf("empty buckets -> %v, want 0", got)
	}
	// 100 requests all inside the first bucket: p95 interpolates inside
	// [0, 5ms] — somewhere in (0, 5ms].
	allFast := fullBuckets(100, 0)
	got := quantileFromBuckets(allFast, 0.95)
	if got <= 0 || got > 5*time.Millisecond {
		t.Errorf("p95 of all-fast = %v, want within (0, 5ms]", got)
	}
	// Half the requests within 5ms, the other half within 500ms: p95 falls
	// in the 500ms bucket's interpolation range (250ms, 500ms].
	mixed := make([]uint64, len(proxy.LatencyBucketBounds))
	for i := 0; i < len(mixed); i++ {
		if i >= 6 { // LatencyBucketBounds[6] = 500ms
			mixed[i] = 100
		} else {
			mixed[i] = 50
		}
	}
	got = quantileFromBuckets(mixed, 0.95)
	if got <= 250*time.Millisecond || got > 500*time.Millisecond {
		t.Errorf("p95 of mixed = %v, want within (250ms, 500ms]", got)
	}
	// Everything between the last two bounds: p95 interpolates inside the
	// last bucket rather than clamping to it.
	slow := make([]uint64, len(proxy.LatencyBucketBounds))
	slow[len(slow)-2] = 2  // ≤5s: 2 requests
	slow[len(slow)-1] = 10 // ≤10s: all 10 requests
	if got := quantileFromBuckets(slow, 0.95); got <= 5*time.Second || got > 10*time.Second {
		t.Errorf("p95 of slow = %v, want within (5s, 10s]", got)
	}
}
