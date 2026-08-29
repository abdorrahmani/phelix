//go:build linux

package update

import "syscall"

// unameMachine returns the kernel machine name (what `uname -m` prints). It
// is only needed on linux/arm, where GOARCH alone cannot distinguish the
// ARMv6 from the ARMv7 release artifact.
func unameMachine() (string, error) {
	var uts syscall.Utsname
	if err := syscall.Uname(&uts); err != nil {
		return "", err
	}
	b := make([]byte, 0, len(uts.Machine))
	for _, c := range uts.Machine {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b), nil
}
