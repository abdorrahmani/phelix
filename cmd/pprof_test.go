package cmd

import (
	"io"
	"net"
	"net/http"
	"runtime"
	"testing"
	"time"
)

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

func TestPprofLifecycle(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	t.Setenv("PHELIX_PPROF", "")
	if stop := startPprofIfEnabled(); stop != nil {
		stop()
		t.Fatal("profiling started without an address")
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PHELIX_PPROF", addr)
	previousFraction := runtime.SetMutexProfileFraction(-1)
	stop := startPprofIfEnabled()
	if stop == nil {
		t.Fatal("profiling did not start")
	}
	defer stop()
	if fraction := runtime.SetMutexProfileFraction(-1); fraction != 10 {
		t.Fatalf("mutex sampling fraction = %d, want 10", fraction)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	for _, path := range []string{"", "heap", "allocs", "goroutine", "block", "mutex", "threadcreate"} {
		response, err := client.Get("http://" + addr + "/debug/pprof/" + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || len(body) == 0 {
			t.Fatalf("profile %s: status=%d, bytes=%d, error=%v", path, response.StatusCode, len(body), err)
		}
	}
	response, err := client.Post("http://"+addr+"/debug/pprof/profile", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", response.StatusCode)
	}
	stop()
	stop()
	if fraction := runtime.SetMutexProfileFraction(-1); fraction != previousFraction {
		t.Errorf("mutex sampling fraction after shutdown = %d, want %d", fraction, previousFraction)
	}
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		conn.Close()
		t.Error("profiling listener remained open after shutdown")
	}
}

func TestPprofOccupiedAddressDoesNotChangeSampling(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("PHELIX_PPROF", listener.Addr().String())
	previousFraction := runtime.SetMutexProfileFraction(-1)
	if stop := startPprofIfEnabled(); stop != nil {
		stop()
		t.Fatal("profiling started on an occupied address")
	}
	if fraction := runtime.SetMutexProfileFraction(-1); fraction != previousFraction {
		t.Errorf("failed startup changed mutex sampling from %d to %d", previousFraction, fraction)
	}
}
