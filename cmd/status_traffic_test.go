package cmd

import (
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/autoscale"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

func TestBuildStatusTraffic(t *testing.T) {
	if got := buildStatusTraffic(nil); got != nil {
		t.Fatalf("expected nil traffic for an unproxied app, got %+v", got)
	}

	got := buildStatusTraffic([]proxy.BackendStat{
		{Label: "replica-0", Host: "127.0.0.1:9001", Requests: 30, Errors: 3},
		{Label: "replica-1", Host: "127.0.0.1:9002", Requests: 10},
	})
	if got.Requests != 40 || got.Errors != 3 {
		t.Fatalf("requests/errors = %d/%d, want 40/3", got.Requests, got.Errors)
	}
	if got.ErrorRate != 7.5 {
		t.Errorf("error rate = %v, want 7.5", got.ErrorRate)
	}
	if len(got.Backends) != 2 {
		t.Fatalf("backends = %d, want 2", len(got.Backends))
	}
	if got.Backends[0].Share != 75 || got.Backends[1].Share != 25 {
		t.Errorf("shares = %v/%v, want 75/25", got.Backends[0].Share, got.Backends[1].Share)
	}
	if got.Backends[0].ErrorRate != 10 || got.Backends[1].ErrorRate != 0 {
		t.Errorf("per-backend rates = %v/%v, want 10/0",
			got.Backends[0].ErrorRate, got.Backends[1].ErrorRate)
	}
}

func TestStatusReplicaResourcesProjection(t *testing.T) {
	sample := autoscale.ReplicaSample{
		Key: "1", PID: 42, ProcessStatus: "running",
		CPUPercent: 12.5, CPUState: autoscale.MetricAvailable,
		MemoryBytes: 128 << 20, MemoryState: autoscale.MetricAvailable,
		MemoryLimit: 512 << 20, MemoryPercent: 25, MemoryPctState: autoscale.MetricAvailable,
		HostMemoryPct: 1.6, HostMemState: autoscale.MetricAvailable,
	}
	var view statusInstanceView
	applyReplicaResources(&view, sample)
	if view.CPUPercent != 12.5 || view.CPUState != "available" {
		t.Errorf("cpu = %v/%s, want 12.5/available", view.CPUPercent, view.CPUState)
	}
	if view.MemoryPercent != 25 || view.MemoryLimitBytes != 512<<20 {
		t.Errorf("memory = %v of %d, want 25 of 512MiB", view.MemoryPercent, view.MemoryLimitBytes)
	}
	line := formatReplicaResources(sample)
	for _, want := range []string{"cpu=12.5%", "mem=128.0MB", "25.0% of limit", "host=1.6%"} {
		if !strings.Contains(line, want) {
			t.Errorf("formatted resources %q missing %q", line, want)
		}
	}

	sample.CPUState = autoscale.MetricInitializing
	sample.MemoryPctState = autoscale.MetricUnavailable
	line = formatReplicaResources(sample)
	if !strings.Contains(line, "cpu=initializing") || strings.Contains(line, "of limit") {
		t.Errorf("unavailable values must be named, not rendered as zero: %q", line)
	}
}
