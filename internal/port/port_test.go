package port

import (
	"net"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestValidate(t *testing.T) {
	valid := []int{1, 80, 3000, 65535}
	for _, p := range valid {
		if err := Validate(p); err != nil {
			t.Errorf("Validate(%d) = %v, want nil", p, err)
		}
	}
	invalid := []int{0, -1, -4000, 65536, 100000}
	for _, p := range invalid {
		if err := Validate(p); err == nil {
			t.Errorf("Validate(%d) = nil, want error", p)
		}
	}
}

func TestParse(t *testing.T) {
	p, err := Parse("3000")
	if err != nil || p != 3000 {
		t.Fatalf("Parse(\"3000\") = %d, %v; want 3000, nil", p, err)
	}
	for _, raw := range []string{"abc", "", "12.5", "0x10", "99999999"} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) = nil error, want error", raw)
		}
	}
	// Negative numbers parse as ints but must fail range validation.
	if _, err := Parse("-1"); err == nil {
		t.Error("Parse(\"-1\") = nil error, want error")
	}
}

func TestEnsureAvailable(t *testing.T) {
	// Occupy a port the kernel hands us, then assert it is unavailable.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on ephemeral port: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if err := EnsureAvailable(int(port)); err == nil {
		t.Errorf("EnsureAvailable(%d) = nil, want PORT_UNAVAILABLE", port)
	} else if code := phelixerr.CodeOf(err); code != phelixerr.CodePortUnavailable {
		t.Errorf("error code = %q, want PORT_UNAVAILABLE", code)
	}
}

func TestWaitForListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on ephemeral port: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	if !WaitForListener(addr, time.Second) {
		t.Errorf("WaitForListener(%q) = false, want true for live listener", addr)
	}
	if WaitForListener("127.0.0.1:1", 300*time.Millisecond) {
		t.Error("WaitForListener on closed port = true, want false")
	}
}
