package deployanalysis

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
)

// ReportInput is the deployment identity and context captured when a
// deployment succeeds. It is persisted with the pending report so a later
// finalization cannot attach the analysis to a different deployment.
type ReportInput struct {
	AnalysisID      string    `json:"analysis_id"`
	DeploymentID    string    `json:"deployment_id"`
	AppName         string    `json:"app_name"`
	AppID           string    `json:"app_id,omitempty"`
	ServerID        string    `json:"server_id,omitempty"`
	Strategy        string    `json:"strategy"`
	PreviousVersion string    `json:"previous_version,omitempty"`
	NewVersion      string    `json:"new_version,omitempty"`
	Outcome         string    `json:"outcome,omitempty"`
	FailureCode     string    `json:"failure_code,omitempty"`
	DeployedAt      time.Time `json:"deployed_at"`
	CompletedAt     time.Time `json:"completed_at"`
	Rollout         *Rollout  `json:"rollout,omitempty"`
}

// PendingReport is a deployment waiting for its post-deployment window to
// finish. The baseline is aggregated at deployment time, while the history is
// still guaranteed to cover it.
type PendingReport struct {
	Input        ReportInput     `json:"input"`
	Baseline     WindowAggregate `json:"baseline"`
	ObserveUntil time.Time       `json:"observe_until"`
	Settings     Config          `json:"settings"`
}

const (
	reportsFileName  = "deployment_reports.json"
	pendingFileName  = "deployment_pending.json"
	maxStoredReports = 100
)

// NewAnalysisID returns the stable identity of one deployment's analysis.
func NewAnalysisID(appName, deploymentID, newVersion string, deployedAt time.Time) string {
	if deploymentID != "" {
		return fmt.Sprintf("%s:%s:%s", appName, deploymentID, newVersion)
	}
	return fmt.Sprintf("%s:%s:%s", appName, newVersion, deployedAt.UTC().Format(time.RFC3339Nano))
}

func reportsPath(appName string) (string, error) {
	dir, err := deploy.AppDataDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, reportsFileName), nil
}

func pendingPath(appName string) (string, error) {
	dir, err := deploy.AppDataDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, pendingFileName), nil
}

// Begin records a successful deployment for later analysis. It is a no-op
// when the deployment identity is incomplete. Analysis failures never
// propagate into the deployment flow: callers may ignore the error, and a
// missing observation window simply yields an insufficient-data report.
func Begin(input ReportInput, cfg Config) error {
	if input.AppName == "" || input.NewVersion == "" {
		return nil
	}
	if input.DeployedAt.IsZero() {
		input.DeployedAt = time.Now().UTC()
	}
	if input.CompletedAt.IsZero() {
		input.CompletedAt = input.DeployedAt
	}
	if input.AnalysisID == "" {
		input.AnalysisID = NewAnalysisID(input.AppName, input.DeploymentID, input.NewVersion, input.DeployedAt)
	}

	baselineStart := input.DeployedAt.Add(-cfg.BaselineWindow)
	baseline, err := AggregateWindow(input.AppName, baselineStart, input.DeployedAt)
	if err != nil {
		baseline = WindowAggregate{StartedAt: baselineStart, EndedAt: input.DeployedAt}
	}

	pending := PendingReport{
		Input:        input,
		Baseline:     baseline,
		ObserveUntil: input.CompletedAt.Add(cfg.ObserveWindow),
		Settings:     cfg,
	}
	return updatePending(input.AppName, func(list []PendingReport) []PendingReport {
		for _, p := range list {
			if p.Input.AnalysisID == input.AnalysisID {
				return list
			}
		}
		return append(list, pending)
	})
}

// FinalizeDue builds reports for every pending deployment whose observation
// window has elapsed. It is safe to call from the monitor tick and from any
// CLI command: pending entries are removed only after their report is durably
// stored.
func FinalizeDue(appName string, cfg Config) ([]Report, error) {
	if appName == "" {
		return nil, nil
	}
	path, err := pendingPath(appName)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	now := time.Now().UTC()
	var created []Report
	err = updatePending(appName, func(list []PendingReport) []PendingReport {
		remaining := make([]PendingReport, 0, len(list))
		for _, p := range list {
			if now.Before(p.ObserveUntil) {
				remaining = append(remaining, p)
				continue
			}
			observed, err := AggregateWindow(appName, p.Input.CompletedAt, p.ObserveUntil)
			if err != nil {
				observed = WindowAggregate{StartedAt: p.Input.CompletedAt, EndedAt: p.ObserveUntil}
			}
			effective := cfg
			if p.Settings.MinSamples > 0 {
				effective = p.Settings
			}
			rep := Analyze(p.Input, p.Baseline, observed, effective)
			if err := SaveReport(appName, rep); err != nil {
				remaining = append(remaining, p)
				continue
			}
			created = append(created, *rep)
		}
		return remaining
	})
	return created, err
}

// PendingCount returns how many analyses are waiting for their window.
func PendingCount(appName string) (int, error) {
	return readPendingCount(appName)
}

func readPendingCount(appName string) (int, error) {
	var count int
	err := withStoreLock(appName, pendingFileName, func() error {
		list, err := loadPendingLocked(appName)
		if err != nil {
			return err
		}
		count = len(list)
		return nil
	})
	return count, err
}

// SaveReport inserts or replaces one report by AnalysisID, newest first, and
// prunes the oldest entries beyond the retention cap.
func SaveReport(appName string, rep *Report) error {
	if appName == "" || rep == nil || rep.AnalysisID == "" {
		return fmt.Errorf("deployanalysis: report has no identity")
	}
	return withStoreLock(appName, reportsFileName, func() error {
		list, err := loadReportsLocked(appName)
		if err != nil {
			return err
		}
		replaced := false
		for i := range list {
			if list[i].AnalysisID == rep.AnalysisID {
				list[i] = *rep
				replaced = true
				break
			}
		}
		if !replaced {
			list = append(list, *rep)
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].AnalyzedAt.Equal(list[j].AnalyzedAt) {
				return list[i].AnalysisID < list[j].AnalysisID
			}
			return list[i].AnalyzedAt.After(list[j].AnalyzedAt)
		})
		if len(list) > maxStoredReports {
			list = list[:maxStoredReports]
		}
		if err := writeReportsLocked(appName, list); err != nil {
			return err
		}
		_ = enqueueSyncLocked(appName, rep)
		return nil
	})
}

// ListReports returns stored reports newest first.
func ListReports(appName string) ([]Report, error) {
	return loadReports(appName)
}

// GetReport returns one stored report, or nil when absent.
func GetReport(appName, analysisID string) (*Report, error) {
	list, err := loadReports(appName)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].AnalysisID == analysisID {
			return &list[i], nil
		}
	}
	return nil, nil
}

// FindByVersion returns the newest report for a target version.
func FindByVersion(appName, version string) (*Report, error) {
	list, err := loadReports(appName)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if strings.EqualFold(list[i].NewVersion, version) {
			return &list[i], nil
		}
	}
	return nil, nil
}

func loadReports(appName string) ([]Report, error) {
	var list []Report
	err := withStoreLock(appName, reportsFileName, func() error {
		var err error
		list, err = loadReportsLocked(appName)
		return err
	})
	return list, err
}

func loadReportsLocked(appName string) ([]Report, error) {
	path, err := reportsPath(appName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var file struct {
		Reports []Report `json:"reports"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		quarantine(path)
		return nil, nil
	}
	return file.Reports, nil
}

func writeReportsLocked(appName string, reports []Report) error {
	path, err := reportsPath(appName)
	if err != nil {
		return err
	}
	payload := struct {
		SchemaVersion int      `json:"schema_version"`
		Reports       []Report `json:"reports"`
	}{SchemaVersion: SchemaVersion, Reports: reports}
	return writeJSONAtomic(path, payload)
}

func updatePending(appName string, mutate func([]PendingReport) []PendingReport) error {
	if appName == "" {
		return nil
	}
	return withStoreLock(appName, pendingFileName, func() error {
		list, err := loadPendingLocked(appName)
		if err != nil {
			return err
		}
		next := mutate(list)
		path, err := pendingPath(appName)
		if err != nil {
			return err
		}
		if len(next) == 0 {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		payload := struct {
			SchemaVersion int             `json:"schema_version"`
			Pending       []PendingReport `json:"pending"`
		}{SchemaVersion: SchemaVersion, Pending: next}
		return writeJSONAtomic(path, payload)
	})
}

func loadPendingLocked(appName string) ([]PendingReport, error) {
	path, err := pendingPath(appName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var file struct {
		Pending []PendingReport `json:"pending"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		quarantine(path)
		return nil, nil
	}
	return file.Pending, nil
}

// writeJSONAtomic writes through a temporary file in the same directory and
// fsyncs before the rename, so a crash leaves either the previous file or the
// complete new one, never a truncated mix.
func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".phelix-analysis-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// quarantine moves an unreadable store aside so the next write starts clean
// while the damaged bytes stay available for inspection.
func quarantine(path string) {
	target := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
	_ = os.Rename(path, target)
}
