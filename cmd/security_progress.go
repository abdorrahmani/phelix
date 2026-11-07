package cmd

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/security"
)

// securityStageLabels map scan stages to the human-facing activity names.
var securityStageLabels = map[string]string{
	"inventory": "Inventorying dependencies",
	"resolve":   "Resolving module versions",
	"query":     "Querying OSV.dev",
	"fetch":     "Fetching advisories",
	"match":     "Evaluating findings",
	"persist":   "Saving scan",
}

// newSecurityProgress renders scan stages on stderr. In an interactive
// terminal the line updates in place; in pipes and CI every stage is one
// plain line, so no terminal escape sequences ever leak into logs or into
// stdout (JSON mode keeps stdout reserved for the envelope regardless).
func newSecurityProgress() security.Progress {
	started := time.Now()
	interactive := IsInteractive()
	var mu sync.Mutex
	lastWidth := 0

	return func(stage, detail string) {
		mu.Lock()
		defer mu.Unlock()
		label, ok := securityStageLabels[stage]
		if !ok {
			label = stage
		}
		elapsed := time.Since(started).Round(100 * time.Millisecond)
		line := fmt.Sprintf("%s: %s (%s)", label, detail, elapsed)
		if !interactive {
			fmt.Fprintln(os.Stderr, line)
			return
		}
		if width := len(line); width < lastWidth {
			line += strings.Repeat(" ", lastWidth-width)
		}
		lastWidth = len(line)
		fmt.Fprintf(os.Stderr, "\r%s", line)
	}
}

// finishSecurityProgress closes the in-place progress line so following
// output starts on a fresh line.
func finishSecurityProgress() {
	if IsInteractive() {
		fmt.Fprintln(os.Stderr)
	}
}
