package grpc

import (
	"sort"
	"testing"
)

// The capability registry exists to tell the backend what THIS agent build
// implements. Its only wire path is CLIMetadata.capabilities, and that field
// was once populated from a hardcoded literal — so the backend saw exactly
// one capability ("build_matrix") no matter what the build supported, and
// the registry itself was dead code. These tests pin both halves: the
// registry contains every implemented feature, and the metadata actually
// carries the registry.

// expectedBuildCapabilities is the full set this build registers. Every
// feature registers its entry from its own implementation file's init():
//
//	monitoring         → monitor_stream.go
//	deployment         → deployment_reporter.go
//	rollback           → rollback_command.go
//	build_matrix       → matrix_command.go
//	webhook_management → webhook_command.go
//
// Adding a remote feature means adding its RegisterCapability call AND its
// entry here — this list is the contract the backend gates on.
var expectedBuildCapabilities = []string{
	CapabilityDeployment,
	CapabilityBuildMatrix,
	CapabilityMonitoring,
	CapabilityRollback,
	CapabilityWebhookManagement,
}

func TestAgentCapabilities_ContainsEveryImplementedFeature(t *testing.T) {
	got := AgentCapabilities()
	want := append([]string(nil), expectedBuildCapabilities...)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("capabilities = %v (%d), want %v (%d)", got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("capabilities = %v, want %v", got, want)
		}
	}
}

func TestAgentCapabilities_SortedAndDeduplicated(t *testing.T) {
	// Registering an existing name twice must not duplicate it.
	RegisterCapability(CapabilityRollback)
	got := AgentCapabilities()
	if !sort.StringsAreSorted(got) {
		t.Fatalf("capabilities are not sorted: %v", got)
	}
	seen := map[string]bool{}
	for _, c := range got {
		if seen[c] {
			t.Fatalf("duplicate capability %q in %v", c, got)
		}
		seen[c] = true
	}
	if len(got) != len(expectedBuildCapabilities) {
		t.Fatalf("re-registering changed the set: %v", got)
	}
}

func TestAgentCapabilities_NeverNil(t *testing.T) {
	// A build with no features must report a non-nil empty list ("baseline
	// agent"), never nil — the backend distinguishes empty from absent.
	saved := make([]string, 0, len(expectedBuildCapabilities))
	for _, c := range AgentCapabilities() {
		saved = append(saved, c)
		unregisterCapability(c)
	}
	t.Cleanup(func() {
		for _, c := range saved {
			RegisterCapability(c)
		}
	})

	got := AgentCapabilities()
	if got == nil {
		t.Fatal("AgentCapabilities() returned nil; want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("capabilities = %v, want empty", got)
	}
}

// TestCollectMetadata_CarriesTheFullRegistry is the regression test for the
// hardcoded-literal bug: the metadata that actually goes on the wire must
// report every registered capability, not a fixed subset.
func TestCollectMetadata_CarriesTheFullRegistry(t *testing.T) {
	setupTestSession(t) // isolated HOME + server identity

	md := collectMetadata()
	if md == nil {
		t.Fatal("collectMetadata returned nil despite an initialized server identity")
	}

	got := md.GetCapabilities()
	want := AgentCapabilities()
	if len(got) != len(want) {
		t.Fatalf("metadata capabilities = %v, want the full registry %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("metadata capabilities = %v, want %v", got, want)
		}
	}

	// Named explicitly so a future regression to a literal fails loudly on
	// the specific entries a backend gates features on.
	for _, required := range expectedBuildCapabilities {
		found := false
		for _, c := range got {
			if c == required {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("metadata capabilities %v is missing %q — the backend would disable that feature", got, required)
		}
	}
}

// TestCollectMetadata_CapabilitiesTrackRegistryChanges proves the metadata
// is computed per call (never a snapshot taken once at startup), so an agent
// that registers a capability later still reports it on the next sync.
func TestCollectMetadata_CapabilitiesTrackRegistryChanges(t *testing.T) {
	setupTestSession(t)

	const probe = "test_probe_capability"
	RegisterCapability(probe)
	t.Cleanup(func() { unregisterCapability(probe) })

	md := collectMetadata()
	if md == nil {
		t.Fatal("collectMetadata returned nil despite an initialized server identity")
	}
	found := false
	for _, c := range md.GetCapabilities() {
		if c == probe {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("metadata capabilities %v does not reflect a newly registered capability", md.GetCapabilities())
	}
}
