package grpc

import (
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
)

// These tests pin the wire contract the backend consumes: every field the
// documentation promises must actually be set, and "unknown" must stay
// distinguishable from a zero value.

func TestToProtoDeploymentEvent_CarriesIdentityAndSnapshot(t *testing.T) {
	now := time.Now()
	ev := deploy.Event{
		AppID:           "42",
		AppName:         "shop",
		DeploymentID:    "dep-1",
		RequestID:       "req-1",
		Event:           deploy.EventProxySwitched,
		Strategy:        "blue-green",
		Phase:           deploy.PhasePromoting,
		Status:          deploy.StatusInProgress,
		CurrentVersion:  "v14",
		TargetVersion:   "v15",
		Slot:            "green",
		InternalPort:    49153,
		PID:             4242,
		ReplicasDesired: 0,
		Message:         "switched",
		Timestamp:       now,
		Snapshot: &deploy.Snapshot{
			AppID:     "42",
			AppName:   "shop",
			RequestID: "req-1",
			Strategy:  "blue-green",

			ActiveSlot: "green",
			Slots: []deploy.SlotState{
				{Slot: "blue", Version: "v14", Status: "draining", Health: deploy.HealthHealthy, InternalPort: 49152, PID: 111},
				{Slot: "green", Version: "v15", Status: "running", Health: deploy.HealthHealthy, InternalPort: 49153, PID: 222, Active: true},
			},
			Proxy: &deploy.ProxyState{
				Enabled: true, PublicPort: 3000, TargetLabel: "green",
				TargetInternalPort: 49153, Upstreams: []string{"127.0.0.1:49153"}, InFlight: 7,
			},
			Health: &deploy.HealthState{
				Mode: "auto", Path: "/healthz", Retries: 5, Interval: "1s",
				Timeout: "30s", Tier: 1, TierLabel: "Tier 1", LastHealthyAt: now,
			},
			UpdatedAt: now,
		},
	}

	out := toProtoDeploymentEvent(ev)
	if out.GetAppId() != "42" || out.GetAppName() != "shop" {
		t.Fatalf("identity = %q/%q", out.GetAppId(), out.GetAppName())
	}
	if out.GetDeploymentId() != "dep-1" {
		t.Fatalf("deployment id = %q", out.GetDeploymentId())
	}
	if out.GetRequestId() != "req-1" || out.GetSnapshot().GetRequestId() != "req-1" {
		t.Fatalf("request correlation event=%q snapshot=%q", out.GetRequestId(), out.GetSnapshot().GetRequestId())
	}
	if out.GetEvent() != deploy.EventProxySwitched {
		t.Fatalf("event = %q", out.GetEvent())
	}
	if out.GetCurrentVersion() != "v14" || out.GetTargetVersion() != "v15" {
		t.Fatalf("versions = %q/%q", out.GetCurrentVersion(), out.GetTargetVersion())
	}
	if out.GetSlot() != "green" || out.GetInternalPort() != 49153 || out.GetPid() != 4242 {
		t.Fatalf("instance fields = %q/%d/%d", out.GetSlot(), out.GetInternalPort(), out.GetPid())
	}
	if out.GetTimestamp() != now.UnixMilli() {
		t.Fatalf("timestamp = %d, want %d", out.GetTimestamp(), now.UnixMilli())
	}
	// The session token must never travel in the body.
	if out.String() != "" && containsToken(out.String()) {
		t.Fatalf("event body appears to carry a token: %s", out.String())
	}

	snap := out.GetSnapshot()
	if snap == nil {
		t.Fatalf("snapshot not attached")
	}
	if len(snap.GetSlots()) != 2 {
		t.Fatalf("slots = %d, want 2", len(snap.GetSlots()))
	}
	var active *string
	for _, sl := range snap.GetSlots() {
		if sl.GetActive() {
			s := sl.GetSlot()
			active = &s
			if sl.GetInternalPort() != 49153 {
				t.Fatalf("active slot port = %d", sl.GetInternalPort())
			}
		}
	}
	if active == nil || *active != "green" {
		t.Fatalf("active slot not reported as green")
	}
	p := snap.GetProxy()
	if p == nil || !p.GetEnabled() || p.GetPublicPort() != 3000 {
		t.Fatalf("proxy = %+v", p)
	}
	if p.GetTargetSlot() != "green" || p.GetTargetInternalPort() != 49153 {
		t.Fatalf("proxy target = %q/%d", p.GetTargetSlot(), p.GetTargetInternalPort())
	}
	if len(p.GetUpstreams()) != 1 || p.GetInFlight() != 7 {
		t.Fatalf("proxy upstreams/in-flight = %v/%d", p.GetUpstreams(), p.GetInFlight())
	}
	h := snap.GetHealth()
	if h == nil || h.GetMode() != "auto" || h.GetPath() != "/healthz" || h.GetTier() != 1 {
		t.Fatalf("health = %+v", h)
	}
	if h.GetRetries() != 5 || h.GetInterval() != "1s" || h.GetTimeout() != "30s" {
		t.Fatalf("health config = %+v", h)
	}
}

func TestToProtoDeploymentEvent_ReplicaEventUsesReplicaFields(t *testing.T) {
	ev := deploy.Event{
		AppID:           "1",
		AppName:         "api",
		DeploymentID:    "dep-2",
		Event:           deploy.EventReplicaHealthy,
		Strategy:        "rolling",
		ReplicaID:       "replica-2",
		ReplicaIndex:    2,
		InternalPort:    50002,
		ReplicasDesired: 3,
		Timestamp:       time.Now(),
		Snapshot: &deploy.Snapshot{
			Strategy:        "rolling",
			ReplicasDesired: 3,
			ReplicasCurrent: 3,
			ReplicasReady:   2,
			ReplicasHealthy: 2,
			Replicas: []deploy.ReplicaState{
				{ID: "replica-0", Index: 0, Status: "running", Health: deploy.HealthHealthy, InternalPort: 50000},
				{ID: "replica-1", Index: 1, Status: "running", Health: deploy.HealthHealthy, InternalPort: 50001},
				{ID: "replica-2", Index: 2, Status: "starting", Health: deploy.HealthPending, InternalPort: 50002},
			},
		},
	}

	out := toProtoDeploymentEvent(ev)
	if out.GetReplicaId() != "replica-2" || out.GetReplicaIndex() != 2 {
		t.Fatalf("replica identity = %q/%d", out.GetReplicaId(), out.GetReplicaIndex())
	}
	// slot is reserved for blue-green; a replica event must leave it empty so
	// the backend never has to guess which dimension applies.
	if out.GetSlot() != "" {
		t.Fatalf("replica event set slot = %q", out.GetSlot())
	}
	if out.GetReplicasDesired() != 3 {
		t.Fatalf("desired = %d", out.GetReplicasDesired())
	}
	snap := out.GetSnapshot()
	if snap.GetReplicasDesired() != 3 || snap.GetReplicasReady() != 2 || snap.GetReplicasHealthy() != 2 {
		t.Fatalf("counts = %d/%d/%d", snap.GetReplicasDesired(), snap.GetReplicasReady(), snap.GetReplicasHealthy())
	}
	if len(snap.GetReplicas()) != 3 {
		t.Fatalf("replicas = %d, want 3", len(snap.GetReplicas()))
	}
	if snap.GetReplicas()[2].GetHealth() != deploy.HealthPending {
		t.Fatalf("starting replica health = %q", snap.GetReplicas()[2].GetHealth())
	}
}

func TestToProtoDeploymentEvent_FailureIsStructured(t *testing.T) {
	ev := deploy.Event{
		Event:          deploy.EventFailed,
		Phase:          deploy.PhaseFailed,
		Status:         deploy.StatusFailed,
		CurrentVersion: "v14",
		TargetVersion:  "v15",
		Failure: &deploy.Failure{
			Code:      "HEALTH_CHECK_FAILED",
			Message:   "probe target: http://127.0.0.1:49153",
			Retryable: true,
		},
		Timestamp: time.Now(),
	}

	out := toProtoDeploymentEvent(ev)
	f := out.GetFailure()
	if f == nil {
		t.Fatalf("failure not converted")
	}
	if f.GetCode() != "HEALTH_CHECK_FAILED" || !f.GetRetryable() {
		t.Fatalf("failure = %+v", f)
	}
	// The old version must still be reported as current after a failure.
	if out.GetCurrentVersion() != "v14" || out.GetTargetVersion() != "v15" {
		t.Fatalf("versions = %q/%q; a failed deploy must not promote", out.GetCurrentVersion(), out.GetTargetVersion())
	}
}

func TestToProtoDeploymentSnapshot_ZeroTimesStayZero(t *testing.T) {
	snap := ToProtoDeploymentSnapshot(&deploy.Snapshot{
		AppName: "x",
		Slots:   []deploy.SlotState{{Slot: "blue", Status: "stopped", Health: deploy.HealthUnknown}},
		Health:  &deploy.HealthState{Tier: 3},
	})
	if snap.GetStartedAt() != 0 || snap.GetUpdatedAt() != 0 {
		t.Fatalf("unset times must serialize as 0, got %d/%d", snap.GetStartedAt(), snap.GetUpdatedAt())
	}
	if snap.GetSlots()[0].GetHealthyAt() != 0 {
		t.Fatalf("a slot that never passed health must have healthy_at 0")
	}
	if snap.GetHealth().GetLastHealthyAt() != 0 {
		t.Fatalf("unset last_healthy_at must be 0")
	}
	// A snapshot with no proxy information must not fabricate one.
	if snap.GetProxy() != nil {
		t.Fatalf("proxy must stay absent when unknown, got %+v", snap.GetProxy())
	}
}

func TestToProtoDeploymentSnapshot_DockerRuntimeCarriesImageAndContainer(t *testing.T) {
	snap := ToProtoDeploymentSnapshot(&deploy.Snapshot{
		AppName: "billing",
		Runtime: "docker",
		Slots: []deploy.SlotState{
			{Slot: "green", Status: "running", Image: "billing:v3", ContainerID: "c0ffee"},
		},
		Replicas: []deploy.ReplicaState{
			{ID: "replica-0", Index: 0, Status: "running", Image: "billing:v3", ContainerID: "dead10cc"},
		},
	})
	if snap.GetRuntime() != "docker" {
		t.Fatalf("runtime = %q, want docker", snap.GetRuntime())
	}
	sl := snap.GetSlots()[0]
	if sl.GetImage() != "billing:v3" || sl.GetContainerId() != "c0ffee" {
		t.Fatalf("slot image/container = %q/%q", sl.GetImage(), sl.GetContainerId())
	}
	rp := snap.GetReplicas()[0]
	if rp.GetImage() != "billing:v3" || rp.GetContainerId() != "dead10cc" {
		t.Fatalf("replica image/container = %q/%q", rp.GetImage(), rp.GetContainerId())
	}
}

func TestToProtoDeploymentSnapshot_NativeLeavesDockerFieldsEmpty(t *testing.T) {
	// A native snapshot: empty runtime, no container id. All three docker fields
	// must serialize empty — empty means "unknown", never "native"/false, so the
	// backend never mistakes a native instance for a container.
	snap := ToProtoDeploymentSnapshot(&deploy.Snapshot{
		AppName: "web",
		Slots:   []deploy.SlotState{{Slot: "blue", Status: "running"}},
	})
	if snap.GetRuntime() != "" {
		t.Fatalf("native runtime must stay empty, got %q", snap.GetRuntime())
	}
	sl := snap.GetSlots()[0]
	if sl.GetImage() != "" || sl.GetContainerId() != "" {
		t.Fatalf("native slot must leave image/container empty, got %q/%q", sl.GetImage(), sl.GetContainerId())
	}
}

func TestToProtoDeploymentSnapshot_NilIsNil(t *testing.T) {
	if got := ToProtoDeploymentSnapshot(nil); got != nil {
		t.Fatalf("nil snapshot must convert to nil, got %+v", got)
	}
	if got := toProtoDeploymentFailure(nil); got != nil {
		t.Fatalf("nil failure must convert to nil, got %+v", got)
	}
}

func TestNewDeploymentSink_NoSessionIsNil(t *testing.T) {
	// No session file in a fresh HOME: telemetry must be off, which is what
	// keeps offline deploys byte-for-byte unchanged.
	t.Setenv("HOME", t.TempDir())
	if sink := NewDeploymentSink(); sink != nil {
		t.Fatalf("without a session the sink must be nil, got %T", sink)
	}
}

// A canary/progressive deployment carries a rollout block (step, intended vs
// observed traffic share, verdict) and a weighted upstream split the plain
// Upstreams host list cannot express. A non-rollout deployment carries neither.
func TestToProtoDeploymentSnapshot_Rollout(t *testing.T) {
	t.Run("rollout and weighted upstreams map onto the wire", func(t *testing.T) {
		now := time.Now()
		out := ToProtoDeploymentSnapshot(&deploy.Snapshot{
			AppName:  "shop",
			Strategy: "canary",
			Proxy: &deploy.ProxyState{
				Enabled: true, PublicPort: 3000,
				Weighted: []deploy.WeightedUpstream{
					{Host: "127.0.0.1:49152", Label: "blue", WeightPercent: 75},
					{Host: "127.0.0.1:49153", Label: "green", WeightPercent: 25},
				},
			},
			Rollout: &deploy.RolloutInfo{
				Strategy:              "canary",
				StepIndex:             1,
				TotalSteps:            4,
				TargetWeightPercent:   25,
				ObservedWeightPercent: 25,
				Status:                "running",
				Decision:              "continue",
				Reason:                "step 2/4",
				Verification: &deploy.RolloutVerificationInfo{
					CanaryErrorRate: 1.5, BaselineErrorRate: 0.5, MaxErrorRate: 5, MaxErrorDelta: 2,
					CanaryP95Ms: 120, BaselineP95Ms: 100, MaxP95Factor: 1.5,
					CanaryRequests: 200, CanaryErrors: 3, BaselineRequests: 800,
					Passed: true,
				},
				StartedAt: now, UpdatedAt: now,
			},
		})
		// __ROLLOUT_ASSERT_PLACEHOLDER__
		r := out.GetRollout()
		if r == nil {
			t.Fatal("a canary deployment must carry a rollout block")
		}
		if r.GetStepIndex() != 1 || r.GetTotalSteps() != 4 {
			t.Fatalf("step = %d/%d, want 1/4", r.GetStepIndex(), r.GetTotalSteps())
		}
		if r.GetTargetWeightPercent() != 25 || r.GetObservedWeightPercent() != 25 {
			t.Fatalf("weights target/observed = %d/%d, want 25/25", r.GetTargetWeightPercent(), r.GetObservedWeightPercent())
		}
		if r.GetStatus() != "running" || r.GetDecision() != "continue" || r.GetAbortCode() != "" {
			t.Fatalf("status/decision/abort = %q/%q/%q", r.GetStatus(), r.GetDecision(), r.GetAbortCode())
		}
		v := r.GetVerification()
		if v == nil {
			t.Fatal("verification block must be carried when metrics were evaluated")
		}
		if v.GetCanaryErrorRate() != 1.5 || v.GetBaselineErrorRate() != 0.5 || v.GetMaxErrorRate() != 5 || v.GetMaxErrorDelta() != 2 {
			t.Fatalf("error-rate verification = %+v", v)
		}
		if v.GetCanaryP95Ms() != 120 || v.GetBaselineP95Ms() != 100 || v.GetMaxP95Factor() != 1.5 {
			t.Fatalf("latency verification = %+v", v)
		}
		if v.GetCanaryRequests() != 200 || v.GetCanaryErrors() != 3 || v.GetBaselineRequests() != 800 {
			t.Fatalf("traffic verification = %+v", v)
		}
		if !v.GetPassed() || v.GetSkipReason() != "" {
			t.Fatalf("verdict = passed:%v skip:%q, want passed with no skip reason", v.GetPassed(), v.GetSkipReason())
		}

		p := out.GetProxy()
		if p == nil {
			t.Fatal("proxy block must be carried")
		}
		w := p.GetWeightedUpstreams()
		if len(w) != 2 {
			t.Fatalf("weighted upstreams = %d, want 2", len(w))
		}
		if w[0].GetHost() != "127.0.0.1:49152" || w[0].GetLabel() != "blue" || w[0].GetWeightPercent() != 75 {
			t.Fatalf("weighted[0] = host:%q label:%q weight:%d", w[0].GetHost(), w[0].GetLabel(), w[0].GetWeightPercent())
		}
		if w[1].GetHost() != "127.0.0.1:49153" || w[1].GetLabel() != "green" || w[1].GetWeightPercent() != 25 {
			t.Fatalf("weighted[1] = host:%q label:%q weight:%d", w[1].GetHost(), w[1].GetLabel(), w[1].GetWeightPercent())
		}
	})

	t.Run("no rollout carries no rollout block", func(t *testing.T) {
		out := ToProtoDeploymentSnapshot(&deploy.Snapshot{
			AppName:  "web",
			Strategy: "blue-green",
			Slots:    []deploy.SlotState{{Slot: "blue", Status: "running"}},
		})
		if out.GetRollout() != nil {
			t.Fatalf("a non-rollout deployment must carry no rollout block, got %+v", out.GetRollout())
		}
	})
}

// containsToken is a coarse check that a serialized message does not embed a
// bearer-looking credential.
func containsToken(s string) bool {
	for _, marker := range []string{"session_token", "Bearer ", "eyJ"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
