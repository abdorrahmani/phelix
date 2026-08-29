//go:build !linux

package update

import "errors"

// unameMachine is only consulted on linux/arm, where GOARCH alone cannot
// distinguish ARMv6 from ARMv7. Other platforms never reach it.
func unameMachine() (string, error) {
	return "", errors.New("uname is not available on this platform")
}
