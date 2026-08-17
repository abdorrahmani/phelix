package auth

import (
	"runtime"
	"time"

	"github.com/abdorrahmani/phelix/internal/version"
)

// authHeaders returns the HTTP headers the CLI sends on its authenticate call,
// per the CLI↔backend auth contract. X-Username, X-API-Key and X-CLI-Version
// are mandatory; the rest are best-effort runtime metadata. The caller adds
// X-Username and X-API-Key alongside these.
func authHeaders() map[string]string {
	h := map[string]string{
		"X-CLI-Version":  version.Version,
		"X-OS":           runtime.GOOS,
		"X-Architecture": runtime.GOARCH,
	}
	if version.BuildTime != "" && version.BuildTime != "unknown" {
		h["X-Build-Time"] = version.BuildTime
	}
	if up := processUptime(); up != "" {
		h["X-Uptime"] = up
	}
	return h
}

// processStart marks when this CLI process began, used to compute X-Uptime.
var processStart = time.Now()

// processUptime returns the CLI process uptime as a Go duration string
// (e.g. "12h34m5s"), or "" when the process has been up for less than a second.
func processUptime() string {
	up := time.Since(processStart)
	if up < time.Second {
		return ""
	}
	return up.Round(time.Second).String()
}
