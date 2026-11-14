package securityscore

import (
	"reflect"
	"testing"
	"time"
)

func TestScore(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	yes := true
	r := Risk{ID: "CVE-2026-1234", Severity: "critical", Confidence: "confirmed", KnownExploited: &yes, Fixed: true}
	s := Calculate([]Risk{r, r}, true, true, now, now)
	if s.Value == nil || *s.Value != 58 || len(s.Breakdown) != 1 {
		t.Fatalf("%+v", s)
	}
	if got := Calculate(nil, false, true, now, now); got.Value != nil {
		t.Fatal("unknown became perfect")
	}
	if got := Calculate(nil, true, true, now, now); got.Value == nil || *got.Value != 100 {
		t.Fatal(got)
	}
	if got := Calculate(nil, true, false, now, now.Add(72*time.Hour)); got.Value == nil || *got.Value != 86 {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(s, Calculate([]Risk{r}, true, true, now, now)) {
		t.Fatal("score not deterministic or double counted")
	}
}
