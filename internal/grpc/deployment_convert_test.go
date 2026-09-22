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
