package workspace

import (
	"reflect"
	"strings"
	"testing"
)

func TestTopoLevels_Table(t *testing.T) {
	cases := []struct {
		name    string
		names   []string
		deps    map[string][]string
		want    [][]string
		wantErr string
	}{
		{
			"no deps single level",
			[]string{"b", "a"},
			map[string][]string{},
			[][]string{{"a", "b"}},
			"",
		},
		{
			"chain",
			[]string{"gateway", "auth", "orders"},
			map[string][]string{
				"gateway": {"auth", "orders"},
				"orders":  {"auth"},
			},
			[][]string{{"auth"}, {"orders"}, {"gateway"}},
			"",
		},
		{
			"diamond",
			[]string{"top", "left", "right", "base"},
			map[string][]string{
				"left":  {"base"},
				"right": {"base"},
				"top":   {"left", "right"},
			},
			[][]string{{"base"}, {"left", "right"}, {"top"}},
			"",
		},
		{
			"unknown dependency",
			[]string{"a", "b"},
			map[string][]string{"a": {"ghost"}},
			nil,
			"unknown service",
		},
		{
			"cycle",
			[]string{"a", "b", "c"},
			map[string][]string{
				"a": {"c"},
				"b": {"a"},
				"c": {"b"},
			},
			nil,
			"cycle",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TopoLevels(tc.names, tc.deps)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("TopoLevels() = %v, %v; want error containing %q", got, err, tc.wantErr)
				}
				if tc.name == "cycle" && !strings.Contains(err.Error(), "->") {
					t.Fatalf("cycle error should trace the cycle: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("TopoLevels: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("TopoLevels() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTopoOrder_FlattensLevels(t *testing.T) {
	deps := map[string][]string{
		"gateway": {"auth", "orders"},
		"orders":  {"auth"},
	}
	got, err := TopoOrder([]string{"gateway", "auth", "orders"}, deps)
	if err != nil {
		t.Fatalf("TopoOrder: %v", err)
	}
	want := []string{"auth", "orders", "gateway"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TopoOrder() = %v, want %v", got, want)
	}
}

func TestDuplicatePort_ReportsClash(t *testing.T) {
	services := []*Service{
		{Name: "auth", Port: 8081},
		{Name: "orders", Port: 8081},
		{Name: "gateway", Port: 8080},
		{Name: "unset"},
	}
	if got := duplicatePort(services); got == "" {
		t.Fatalf("duplicatePort() = %q, want a clash message", got)
	}
	if got := duplicatePort(services[:1]); got != "" {
		t.Fatalf("duplicatePort(single) = %q, want empty", got)
	}
}
