package hostsecurity

import (
	"context"
	"errors"
	"time"

	"github.com/abdorrahmani/phelix/internal/securityscore"
	"github.com/abdorrahmani/phelix/internal/version"
	"github.com/google/uuid"
)

func Collect(ctx context.Context, system System, store *Store, serverID string, progress func(string)) (Scan, error) {
	return ScanHost(ctx, system, store, serverID, progress, nil)
}

func ScanHost(ctx context.Context, system System, store *Store, serverID string, progress func(string), intel *Intelligence) (Scan, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	scan := Scan{ID: uuid.NewString(), ServerID: serverID, Domain: "server", StartedAt: time.Now().UTC(), ScannerVersion: version.Version, MethodVersion: "host-inventory-v1", Status: "failed", Coverage: "unknown", Warnings: []string{}}
	if progress != nil {
		progress("Detecting operating system")
	}
	h, err := system.Detect(ctx)
	scan.Host = h
	var packages []Package
	if err == nil && h.Status == "supported" {
		if progress != nil {
			progress("Reading installed packages")
		}
		packages, scan.SnapshotID, err = system.Inventory(ctx, h, scan.StartedAt)
	} else if err == nil {
		scan.Status = h.Status
	}
	if err == nil && h.Status == "supported" {
		scan.Status = "inventoried"
		scan.Warnings = append(scan.Warnings, "Vulnerability intelligence has not been evaluated")
	}
	if err != nil {
		scan.Warnings = append(scan.Warnings, err.Error())
		if errors.Is(err, context.Canceled) {
			scan.Status = "cancelled"
		}
	}
	if err == nil && h.Status == "supported" && intel != nil {
		if progress != nil {
			progress("Evaluating distribution advisories")
		}
		var complete bool
		var unknown int
		scan.Findings, scan.Sources, complete, unknown, err = intel.Match(ctx, system, h, packages)
		scan.UnknownPackages = unknown
		scan.Status = "completed"
		scan.MethodVersion = "host-match-v1"
		scan.Warnings = nil
		if err != nil {
			scan.Warnings = append(scan.Warnings, err.Error())
		}
		if !complete {
			scan.Status = "partial"
			scan.Coverage = "partial"
		} else {
			scan.Coverage = "complete"
		}
		fresh := true
		for _, source := range scan.Sources {
			if source.Stale {
				fresh = false
			}
			if source.Error != "" {
				scan.Warnings = append(scan.Warnings, source.Name+": "+source.Error)
			}
		}
		risks := []securityscore.Risk{}
		scan.Counts = map[string]int{}
		for _, finding := range scan.Findings {
			risks = append(risks, finding.Risk)
			scan.Counts[finding.Severity]++
		}
		scan.Score = securityscore.Calculate(risks, complete, fresh, scan.StartedAt, time.Now().UTC())
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			err = nil
		}
	}
	scan.PackageCount = len(packages)
	scan.FinishedAt = time.Now().UTC()
	scan.DurationMS = scan.FinishedAt.Sub(scan.StartedAt).Milliseconds()
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer saveCancel()
	if saveErr := store.SaveScan(saveCtx, scan, packages); saveErr != nil {
		return scan, errors.Join(err, saveErr)
	}
	return scan, err
}
