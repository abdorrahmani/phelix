package server

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// getServerRegion attempts to determine the server region from environment or cloud metadata
func getServerRegion() string {
	// Check common cloud provider environment variables
	if region := os.Getenv("AWS_REGION"); region != "" {
		return region
	}
	if region := os.Getenv("REGION"); region != "" {
		return region
	}
	if zone := os.Getenv("CLOUD_ZONE"); zone != "" {
		return zone
	}
	return "unknown"
}

// getArchitecture returns the system architecture
func getArchitecture() string {
	return runtime.GOARCH
}

// getKernelVersion returns the kernel version
func getKernelVersion() string {
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}
