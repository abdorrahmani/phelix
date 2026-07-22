package version

import (
	"fmt"
	"runtime"
)

var (
	Version   = "0.0.0-dev"
	BuildID   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"

	CGOEnabled = "unknown"
)

func Platform() string {
	return fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
}

func Compiler() string {
	return runtime.Compiler
}
