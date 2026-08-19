package auth

import "github.com/abdorrahmani/phelix/internal/logs"

// setupLogging installs the leveled, file-backed self logger shared by the
// whole CLI. It is called during package init (from auth.go) so that every
// command's log output lands in ~/.phelix/logs/phelix.log with the standard
// "ts [LEVEL] [component] message" format — the same file the monitor daemon
// tails and forwards to the backend.
func setupLogging() {
	logs.InitFileLog()
}
