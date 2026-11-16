package server

import "testing"

func BenchmarkHostMetrics(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := collectMetricsOnce(); err != nil {
			b.Fatal(err)
		}
	}
}
