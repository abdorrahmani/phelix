package cmd

import (
	"testing"
	"time"
)

func TestLinePredatesJSONRecord(t *testing.T) {
	since := time.Date(2026, 10, 29, 18, 0, 0, 0, time.UTC)

	old := `{"ts":"2026-10-29T17:59:59.000Z","source":"app","msg":"old"}`
	if !linePredates(old, since) {
		t.Errorf("record before --since must be filtered")
	}

	fresh := `{"ts":"2026-10-29T18:00:01.000Z","source":"app","msg":"new"}`
	if linePredates(fresh, since) {
		t.Errorf("record after --since must be kept")
	}
}
