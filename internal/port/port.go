// Package port implements Phelix's port contract: validate a requested port,
// check availability, and verify at runtime that an application actually
// listens on the port it was given via PORT.
package port

import (
	"fmt"
	"net"
	"strconv"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Validate checks that port is in the usable TCP range 1-65535.
func Validate(port int) error {
	if port <= 0 || port > 65535 {
		return phelixerr.Newf(
			phelixerr.CodeValidation,
			"invalid port %d: must be between 1 and 65535",
			port,
		)
	}
	return nil
}

// Parse converts a raw string port into an int, rejecting non-numeric and
// malformed values with the same structured error as Validate.
func Parse(raw string) (int, error) {
	p, err := strconv.Atoi(raw)
	if err != nil {
		return 0, phelixerr.Newf(
			phelixerr.CodeValidation,
			"invalid port %q: not a number",
			raw,
		)
	}
	return p, Validate(p)
}

// IsAvailable reports whether something is already listening on 127.0.0.1:port.
func IsAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// EnsureAvailable returns a PORT_UNAVAILABLE error when the port is occupied.
func EnsureAvailable(port int) error {
	if !IsAvailable(port) {
		return phelixerr.Newf(
			phelixerr.CodePortUnavailable,
			"port %d is already in use by another process",
			port,
		)
	}
	return nil
}

// IsListening reports whether something accepts TCP connections on addr right
// now (single probe, no retry).
func IsListening(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// WaitForListener polls addr until a TCP connection succeeds or timeout
// elapses. It verifies the application actually listens, not merely that the
// process exists.
func WaitForListener(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if IsListening(addr) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
