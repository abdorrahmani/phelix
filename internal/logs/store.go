package logs

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// logStoreRetention is how long stored records are kept.
const logStoreRetention = 30 * 24 * time.Hour

// LogStore is the opt-in long-term log store: a local SQLite database
// indexed by the fields every incident query starts from. It is pure Go
// (modernc.org/sqlite, no cgo) and disabled unless the daemon runs with
// PHELIX_LOG_STORE=1.
type LogStore struct {
	db *sql.DB
	mu sync.Mutex
}

// LogStoreEnabled reports whether long-term log storage is opted in.
func LogStoreEnabled() bool {
	v := os.Getenv("PHELIX_LOG_STORE")
	return v == "1" || strings.EqualFold(v, "true")
}

// OpenLogStore opens and initializes the store under <dataDir>/logs/records.db.
func OpenLogStore() (*LogStore, error) {
	path := filepath.Join(dataDir(), "logs", "records.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	s := &LogStore{db: db}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *LogStore) init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS log_records (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts INTEGER NOT NULL,
			level TEXT NOT NULL,
			source TEXT NOT NULL,
			app TEXT NOT NULL DEFAULT '',
			app_id TEXT NOT NULL DEFAULT '',
			component TEXT NOT NULL DEFAULT '',
			version TEXT NOT NULL DEFAULT '',
			deploy_id TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL DEFAULT '',
			stream TEXT NOT NULL DEFAULT '',
			msg TEXT NOT NULL,
			fields TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_log_records_ts ON log_records(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_log_records_app ON log_records(app)`,
		`CREATE INDEX IF NOT EXISTS idx_log_records_level ON log_records(level)`,
		`CREATE INDEX IF NOT EXISTS idx_log_records_version ON log_records(version)`,
		`CREATE INDEX IF NOT EXISTS idx_log_records_request ON log_records(request_id)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// Insert stores records; records with an unparseable timestamp are stamped
// with the insert time so nothing is silently dropped.
func (s *LogStore) Insert(records []LogRecord) error {
	if len(records) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO log_records
		(ts, level, source, app, app_id, component, version, deploy_id, request_id, stream, msg, fields)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, r := range records {
		ts := ParseRecordTime(r.TS)
		if ts.IsZero() {
			ts = time.Now()
		}
		fields := ""
		if len(r.Fields) > 0 {
			if b, err := json.Marshal(r.Fields); err == nil {
				fields = string(b)
			}
		}
		if _, err := stmt.Exec(
			ts.UnixMilli(), string(r.Level), r.Source, r.App, r.AppID,
			r.Component, r.Version, r.DeployID, r.RequestID, string(r.Stream),
			r.Msg, fields,
		); err != nil {
			stmt.Close()
			tx.Rollback()
			return err
		}
	}
	stmt.Close()
	return tx.Commit()
}

// Query returns up to limit records matching the filters, newest first.
// Empty filter values match everything; since.IsZero disables the time
// bound.
func (s *LogStore) Query(app string, level LogLevel, version, requestID string, since time.Time, limit int) ([]LogRecord, error) {
	where := []string{"1=1"}
	args := []any{}
	if app != "" {
		where = append(where, "app = ?")
		args = append(args, app)
	}
	if level != "" {
		where = append(where, "level = ?")
		args = append(args, string(normalizeLevel(level)))
	}
	if version != "" {
		where = append(where, "version = ?")
		args = append(args, version)
	}
	if requestID != "" {
		where = append(where, "request_id = ?")
		args = append(args, requestID)
	}
	if !since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, since.UnixMilli())
	}
	if limit <= 0 {
		limit = 200
	}

	rows, err := s.db.Query(
		`SELECT ts, level, source, app, app_id, component, version, deploy_id,
			request_id, stream, msg, fields
		FROM log_records WHERE `+strings.Join(where, " AND ")+`
		ORDER BY ts DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogRecord
	for rows.Next() {
		var ts int64
		var level, source, stream, fieldsJSON string
		var r LogRecord
		if err := rows.Scan(&ts, &level, &source, &r.App, &r.AppID, &r.Component,
			&r.Version, &r.DeployID, &r.RequestID, &stream, &r.Msg, &fieldsJSON); err != nil {
			return nil, err
		}
		r.TS = time.UnixMilli(ts).UTC().Format(RFC3339Milli)
		r.Level = LogLevel(level)
		r.Source = source
		r.Stream = LogStream(stream)
		if fieldsJSON != "" {
			_ = json.Unmarshal([]byte(fieldsJSON), &r.Fields)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Retain deletes records older than the retention window and reports how
// many were removed.
func (s *LogStore) Retain() (int64, error) {
	res, err := s.db.Exec("DELETE FROM log_records WHERE ts < ?",
		time.Now().Add(-logStoreRetention).UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Close releases the database handle.
func (s *LogStore) Close() error { return s.db.Close() }

var (
	storeOnce sync.Once
	logStore  *LogStore
)

// StoreRecords persists records into the long-term store when it is enabled;
// it is a no-op otherwise. Initialization happens once: on failure the store
// stays disabled for the process lifetime and the failure is logged once.
func StoreRecords(records []LogRecord) {
	if len(records) == 0 || !LogStoreEnabled() {
		return
	}
	storeOnce.Do(func() {
		s, err := OpenLogStore()
		if err != nil {
			Error("logs", "long-term log store disabled: %v", err)
			return
		}
		logStore = s
	})
	if logStore == nil {
		return
	}
	if err := logStore.Insert(records); err != nil {
		Error("logs", "failed to store log records: %v", err)
	}
}

// StartStoreRetention runs the 30-day retention sweep once an hour until
// stop is closed. Call only when the store is enabled.
func StartStoreRetention(stop <-chan struct{}) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if logStore == nil {
				continue
			}
			if n, err := logStore.Retain(); err != nil {
				Error("logs", "log store retention failed: %v", err)
			} else if n > 0 {
				InfoFile("logs", "log store retention removed %d records", n)
			}
		}
	}
}
