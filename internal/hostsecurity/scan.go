package hostsecurity

import (
	"context"
	"errors"
	"time"

	"github.com/abdorrahmani/phelix/internal/version"
	"github.com/google/uuid"
)

func Collect(ctx context.Context, system System, store *Store, serverID string, progress func(string)) (Scan, error) {
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
