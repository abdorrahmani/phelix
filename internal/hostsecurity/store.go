package hostsecurity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct{ DB *sql.DB }

func OpenStore(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "host-security")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "security.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS host_scans(id TEXT PRIMARY KEY, server_id TEXT NOT NULL, started_at TEXT NOT NULL, status TEXT NOT NULL, snapshot_id TEXT NOT NULL, record BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS host_scans_history ON host_scans(server_id,started_at DESC,id);
CREATE TABLE IF NOT EXISTS host_packages(snapshot_id TEXT NOT NULL, package_key TEXT NOT NULL, name TEXT NOT NULL, record BLOB NOT NULL, PRIMARY KEY(snapshot_id,package_key));
CREATE INDEX IF NOT EXISTS host_packages_name ON host_packages(snapshot_id,name,package_key);
CREATE TRIGGER IF NOT EXISTS host_scans_immutable BEFORE UPDATE ON host_scans BEGIN SELECT RAISE(ABORT,'completed scans are immutable'); END;`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db}, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) SaveScan(ctx context.Context, scan Scan, packages []Package) error {
	if scan.ID == "" || scan.ServerID == "" || scan.Status == "running" || scan.FinishedAt.IsZero() || scan.PackageCount != len(packages) {
		return errors.New("invalid completed host scan")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range packages {
		raw, e := json.Marshal(p)
		if e != nil {
			return e
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO host_packages(snapshot_id,package_key,name,record) VALUES(?,?,?,?)`, scan.SnapshotID, p.Key, p.Name, raw); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(scan)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO host_scans(id,server_id,started_at,status,snapshot_id,record) VALUES(?,?,?,?,?,?)`, scan.ID, scan.ServerID, scan.StartedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"), scan.Status, scan.SnapshotID, raw); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Scan(ctx context.Context, serverID, id string) (Scan, error) {
	var raw []byte
	var err error
	if id == "" {
		err = s.DB.QueryRowContext(ctx, `SELECT record FROM host_scans WHERE server_id=? ORDER BY started_at DESC,id DESC LIMIT 1`, serverID).Scan(&raw)
	} else {
		err = s.DB.QueryRowContext(ctx, `SELECT record FROM host_scans WHERE server_id=? AND id=?`, serverID, id).Scan(&raw)
	}
	var scan Scan
	if err != nil {
		return scan, err
	}
	err = json.Unmarshal(raw, &scan)
	return scan, err
}

// LatestFinished returns the newest completed or partial scan record for a
// server. The scheduler anchors its completion-anchored cadence on it.
func (s *Store) LatestFinished(ctx context.Context, serverID string) (Scan, error) {
	var raw []byte
	err := s.DB.QueryRowContext(ctx, `SELECT record FROM host_scans WHERE server_id=? AND status IN ('completed','partial') ORDER BY started_at DESC,id DESC LIMIT 1`, serverID).Scan(&raw)
	var scan Scan
	if err != nil {
		return scan, err
	}
	err = json.Unmarshal(raw, &scan)
	return scan, err
}

func ValidatePage(limit, offset int) error {
	if limit < 1 || limit > 500 || offset < 0 || offset > 1000000 {
		return errors.New("limit must be 1..500 and offset 0..1000000")
	}
	return nil
}

func (s *Store) History(ctx context.Context, serverID string, limit, offset int) (Page[Scan], error) {
	p := Page[Scan]{Items: []Scan{}, Limit: limit, Offset: offset}
	if err := ValidatePage(limit, offset); err != nil {
		return p, err
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM host_scans WHERE server_id=?`, serverID).Scan(&p.Total); err != nil {
		return p, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT record FROM host_scans WHERE server_id=? ORDER BY started_at DESC,id DESC LIMIT ? OFFSET ?`, serverID, limit, offset)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var scan Scan
		if err = rows.Scan(&raw); err != nil {
			return p, err
		}
		if err = json.Unmarshal(raw, &scan); err != nil {
			return p, err
		}
		p.Items = append(p.Items, scan)
	}
	return p, rows.Err()
}

// Inventory lists the packages of one snapshot. The search is a
// case-insensitive substring match, aligned with the report --package
// semantics.
func (s *Store) Inventory(ctx context.Context, serverID, scanID, search string, limit, offset int) (Page[Package], error) {
	p := Page[Package]{Items: []Package{}, Limit: limit, Offset: offset}
	if err := ValidatePage(limit, offset); err != nil {
		return p, err
	}
	scan, err := s.Scan(ctx, serverID, scanID)
	if err != nil {
		return p, err
	}
	if err = s.DB.QueryRowContext(ctx, `SELECT count(*) FROM host_packages WHERE snapshot_id=? AND instr(lower(name),lower(?))>0`, scan.SnapshotID, search).Scan(&p.Total); err != nil {
		return p, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT record FROM host_packages WHERE snapshot_id=? AND instr(lower(name),lower(?))>0 ORDER BY package_key LIMIT ? OFFSET ?`, scan.SnapshotID, search, limit, offset)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var pkg Package
		if err = rows.Scan(&raw); err != nil {
			return p, err
		}
		if err = json.Unmarshal(raw, &pkg); err != nil {
			return p, err
		}
		pkg.InventoryAt = scan.FinishedAt
		p.Items = append(p.Items, pkg)
	}
	return p, rows.Err()
}
