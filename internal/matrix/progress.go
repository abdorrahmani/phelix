package matrix

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type progressContextKey struct{}

type progressReporter func(key, stage string, current, total int64)

type progressOutputWriter struct {
	ctx   context.Context
	key   string
	debug bool
	log   *bytes.Buffer
}

func (w *progressOutputWriter) Write(data []byte) (int, error) {
	if w.log != nil {
		w.log.Write(data)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ReportBuildProgress(w.ctx, w.key, progressStage(line), 0, 0)
		if w.debug {
			fmt.Fprintf(os.Stderr, "%s\n", line)
		}
	}
	return len(data), nil
}

func progressStage(line string) string {
	if _, section, found := cutDockerStep(line); found {
		return section
	}
	fields := strings.Fields(line)
	if len(fields) > 1 && (fields[0] == "Compiling" || fields[0] == "Downloading" || fields[0] == "Finished") {
		return strings.ToLower(fields[0])
	}
	return ""
}

func cutDockerStep(line string) (number, section string, found bool) {
	if !strings.HasPrefix(line, "#") {
		if fields := strings.Fields(line); len(fields) >= 3 && strings.HasPrefix(fields[1], "/") {
			return fields[0], fields[2], true
		}
		return "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(line, "#"), " ", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[1], "[") {
		return "", "", false
	}
	end := strings.Index(parts[1], "]")
	if end < 0 {
		return "", "", false
	}
	return parts[0], parts[1][1:end], true
}

// WithBuildProgress attaches stage reporting to a matrix build context.
func WithBuildProgress(ctx context.Context, key string, report progressReporter) context.Context {
	return context.WithValue(ctx, progressContextKey{}, func(reportKey, stage string, current, total int64) {
		if report != nil {
			report(reportKey, stage, current, total)
		}
	})
}

// ReportBuildProgress publishes an optional build-stage update. A zero total
// means that the operation does not expose a deterministic percentage.
func ReportBuildProgress(ctx context.Context, key, stage string, current, total int64) {
	reporter, ok := ctx.Value(progressContextKey{}).(progressReporter)
	if ok {
		reporter(key, stage, current, total)
	}
}

type progressBarState struct {
	label   string
	stage   string
	current int64
	total   int64
	started time.Time
	status  string
}

// MatrixProgress renders one status cell per combination. Cells are laid out
// two across so concurrent matrix builds remain visible simultaneously.
type MatrixProgress struct {
	mu      sync.Mutex
	states  map[string]*progressBarState
	order   []string
	out     io.Writer
	renderd bool
	done    chan struct{}
	stopped sync.Once
}

func NewMatrixProgress(combinations []Combination, out io.Writer) *MatrixProgress {
	if out == nil {
		out = os.Stdout
	}
	progress := &MatrixProgress{
		states: make(map[string]*progressBarState, len(combinations)),
		out:    out,
		done:   make(chan struct{}),
	}
	for _, combination := range combinations {
		key := combination.ID()
		progress.order = append(progress.order, key)
		progress.states[key] = &progressBarState{label: key, started: time.Now(), status: "queued"}
	}
	return progress
}

func (p *MatrixProgress) Start() {
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-p.done:
				return
			case <-ticker.C:
				p.Render()
			}
		}
	}()
}

func (p *MatrixProgress) Update(key, stage string, current, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, ok := p.states[key]
	if !ok || state.status == "done" || state.status == "failed" {
		return
	}
	state.stage = stage
	state.current = current
	state.total = total
	state.status = "running"
	if state.started.IsZero() {
		state.started = time.Now()
	}
	p.renderLocked()
}

func (p *MatrixProgress) Finish(key, status string) {
	p.mu.Lock()
	if state := p.states[key]; state != nil {
		state.status = status
		state.stage = ""
		p.renderLocked()
	}
	p.mu.Unlock()
}

func (p *MatrixProgress) Stop() {
	p.stopped.Do(func() { close(p.done) })
	p.mu.Lock()
	if p.renderd {
		fmt.Fprint(p.out, "\r\033[J")
	}
	p.mu.Unlock()
}

func (p *MatrixProgress) Render() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.renderLocked()
}

func (p *MatrixProgress) renderLocked() {
	cells := make([]string, 0, len(p.order))
	for _, key := range p.order {
		cells = append(cells, renderProgressCell(p.states[key]))
	}
	columns := 2
	rows := make([]string, 0, (len(cells)+columns-1)/columns)
	for i := 0; i < len(cells); i += columns {
		end := min(i+columns, len(cells))
		row := cells[i]
		for _, cell := range cells[i+1 : end] {
			row += "  " + cell
		}
		rows = append(rows, row)
	}

	body := strings.Join(rows, "\n")
	if p.renderd {
		fmt.Fprintf(p.out, "\r\033[%dA\r\033[J%s", len(rows)-1, body)
	} else {
		fmt.Fprint(p.out, body)
		p.renderd = true
	}
}

func renderProgressCell(state *progressBarState) string {
	const width = 38
	label := state.label
	if len(label) > 20 {
		label = label[:17] + "..."
	}

	percent := 0
	detail := state.status
	if state.status == "running" {
		if state.total > 0 {
			percent = int(float64(state.current) / float64(state.total) * 100)
			detail = fmt.Sprintf("%s %s/%s", state.stage, formatBytes(state.current), formatBytes(state.total))
		} else if state.stage != "" {
			detail = state.stage
		}
	} else if state.status == "done" {
		percent = 100
	} else if state.status == "failed" {
		detail = "failed"
	}
	percent = max(0, min(100, percent))

	filled := percent * 10 / 100
	bar := strings.Repeat("=", filled) + strings.Repeat(" ", 10-filled)
	cell := fmt.Sprintf("%-20s [%s] %3d%% %-12s", label, bar, percent, detail)
	if len(cell) < width {
		cell += strings.Repeat(" ", width-len(cell))
	}
	return cell
}

func formatBytes(value int64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%dB", value)
	}
	units := []string{"KiB", "MiB", "GiB"}
	size := float64(value)
	index := -1
	for size >= unit && index < len(units)-1 {
		size /= unit
		index++
	}
	return fmt.Sprintf("%.1f%s", size, units[index])
}

// ProgressBar renders a single indeterminate or byte-counted operation.
type ProgressBar struct {
	mu       sync.Mutex
	label    string
	stage    string
	current  int64
	total    int64
	started  time.Time
	out      io.Writer
	rendered bool
	done     chan struct{}
	stopped  sync.Once
}

func NewProgressBar(label string, out io.Writer) *ProgressBar {
	if out == nil {
		out = os.Stdout
	}
	return &ProgressBar{label: label, started: time.Now(), out: out, done: make(chan struct{})}
}

func (p *ProgressBar) Start() {
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-p.done:
				return
			case <-ticker.C:
				p.mu.Lock()
				p.render()
				p.mu.Unlock()
			}
		}
	}()
}

func (p *ProgressBar) Update(stage string, current, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stage = stage
	p.current = current
	p.total = total
	p.render()
}

func (p *ProgressBar) Finish(status string) {
	p.mu.Lock()
	fmt.Fprintf(p.out, "\r\033[K%s %s (%s)\n", p.label, status, time.Since(p.started).Round(time.Millisecond))
	p.rendered = false
	p.mu.Unlock()
	p.stopped.Do(func() { close(p.done) })
}

func (p *ProgressBar) render() {
	state := &progressBarState{
		label:   p.label,
		stage:   p.stage,
		current: p.current,
		total:   p.total,
		started: p.started,
		status:  "running",
	}
	if p.total > 0 {
		state.current = p.current
	} else {
		state.stage = p.stage + " " + time.Since(p.started).Round(time.Second).String()
	}
	fmt.Fprintf(p.out, "\r\033[K%s", strings.TrimRight(renderProgressCell(state), " "))
	p.rendered = true
}
