package resources

import "testing"

func TestParseMemoryEvents(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    string
		want    MemoryEvents
		wantErr bool
	}{
		{"zero counters", "oom 0\noom_kill 0\n", MemoryEvents{0, 0}, false},
		{"first kill", "oom 1\noom_kill 1\n", MemoryEvents{1, 1}, false},
		{"large counters", "oom 18446744073709551615\noom_kill 9007199254740993\n", MemoryEvents{18446744073709551615, 9007199254740993}, false},
		{"reordered with extra kernel keys", "oom_kill 7\noom_group_kill 2\noom 5\n", MemoryEvents{5, 7}, false},
		{"missing oom_kill", "oom 1\n", MemoryEvents{}, true},
		{"missing oom", "oom_kill 1\n", MemoryEvents{}, true},
		{"empty file", "", MemoryEvents{}, true},
		{"malformed value", "oom x\noom_kill 1\n", MemoryEvents{}, true},
		{"malformed line", "oom\noom_kill 1\n", MemoryEvents{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMemoryEvents([]byte(tc.data))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseCPUStat(t *testing.T) {
	good := "usage_usec 1000\nuser_usec 600\nsystem_usec 400\nnr_periods 10\nnr_throttled 3\nthrottled_usec 25\n"
	want := CPUStat{1000, 600, 400, 10, 3, 25}
	got, err := parseCPUStat([]byte(good))
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
	// Kernels add counters over time; unknown keys must not break parsing.
	got, err = parseCPUStat([]byte("nr_migrations 2\n" + good))
	if err != nil || got != want {
		t.Fatalf("unknown key broke parse: %+v, %v", got, err)
	}
	if _, err := parseCPUStat([]byte("usage_usec 1000\nuser_usec 600\nsystem_usec 400\nnr_periods 10\nnr_throttled 3\n")); err == nil {
		t.Fatal("missing throttled_usec accepted")
	}
	if _, err := parseCPUStat([]byte(good + "garbage\n")); err == nil {
		t.Fatal("malformed line accepted")
	}
}

func TestParseScalarControl(t *testing.T) {
	for _, tc := range []struct {
		data    string
		want    uint64
		wantErr bool
	}{
		{"536870912\n", 536870912, false},
		{"max\n", 0, false},
		{"  42  \n", 42, false},
		{"-1\n", 0, true},
		{"abc\n", 0, true},
		{"1 2\n", 0, true},
	} {
		got, err := parseScalarControl([]byte(tc.data))
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Fatalf("%q = %d, %v; want %d, err %v", tc.data, got, err, tc.want, tc.wantErr)
		}
	}
}
