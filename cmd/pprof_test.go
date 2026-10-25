package cmd

import "testing"

func TestNormalizePprofAddr(t *testing.T) {
	cases := map[string]string{
		"6060":             "127.0.0.1:6060",
		":6060":            "127.0.0.1:6060",
		"127.0.0.1:6060":   "127.0.0.1:6060",
		"0.0.0.0:6060":     "0.0.0.0:6060",
		"192.168.1.5:6060": "192.168.1.5:6060",
	}
	for in, want := range cases {
		if got := normalizePprofAddr(in); got != want {
			t.Errorf("normalizePprofAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "127.1.2.3", "::1", "[::1]"} {
		if !isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"", "0.0.0.0", "192.168.1.5", "example.com"} {
		if isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = true, want false", host)
		}
	}
}
