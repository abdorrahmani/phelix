package monitor

import (
	"github.com/abdorrahmani/phelix/internal/deploy"
	"testing"
	"time"
)

func TestRuntimeCacheFreshness(t *testing.T) {
	cache := NewRuntimeMetricsCache()
	cache.samples["id"] = deploy.RuntimeMetrics{ContainerID: "id", CPUPercent: 30}
	cache.collected = time.Now()
	if m, ok := cache.Get("id"); !ok || m.CPUPercent != 30 {
		t.Fatal(m, ok)
	}
	cache.collected = time.Now().Add(-time.Minute)
	if _, ok := cache.Get("id"); ok {
		t.Fatal("stale metric reported current")
	}
	if _, ok := cache.Get("missing"); ok {
		t.Fatal("missing metric reported")
	}
}
