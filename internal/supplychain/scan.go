package supplychain

import (
	"context"
	"errors"
	"github.com/abdorrahmani/phelix/internal/security"
	"strconv"
	"time"
)

func Rescan(ctx context.Context, s *Store, r *Report) error {
	return rescan(ctx, s, r, true)
}

func rescan(ctx context.Context, s *Store, r *Report, persist bool) error {
	save := func() error {
		if !persist {
			return nil
		}
		return s.Save(r)
	}
	store := security.NewScanStore(s.DataDir)
	identity := r.AppID + "-v" + strconv.Itoa(r.Version) + "-" + r.Artifact.SHA256[:12]
	if r.BuildID != "" {
		identity += "-" + Digest([]byte(r.BuildID))[:32]
	}
	lock, err := store.TryAcquireScanLock(identity)
	if err != nil {
		return err
	}
	defer lock.Release()
	result, err := security.RunScan(ctx, security.ScanOptions{AppID: r.AppID, AppName: r.AppName, IdentityKey: identity, DataDir: s.DataDir, Trigger: security.TriggerManual, Inventory: r.Inventory, OSVBaseURL: security.OSVBaseURL()})
	if err != nil {
		records, listErr := store.ListScans(identity)
		if listErr == nil && len(records) > 0 {
			r.Scan = records[0]
			return errors.Join(errors.New("artifact vulnerability scan failed; findings are unavailable"), save())
		}
		now := time.Now().UTC()
		r.Scan = &security.ScanRecord{Status: security.ScanFailed, AppID: r.AppID, AppName: r.AppName, IdentityKey: identity, StartedAt: now, FinishedAt: &now, Outcome: "vulnerability scanner unavailable", Coverage: security.CoveragePartial, Freshness: security.FreshnessStale}
		return errors.Join(errors.New("artifact vulnerability scan failed; findings are unavailable"), save())
	}
	r.Scan = result.Record
	p, err := s.LoadPolicy(r.AppID)
	if err != nil {
		return err
	}
	r.Policy = p
	r.Decisions, err = Evaluate(r, p)
	saveErr := save()
	return errors.Join(err, saveErr)
}
