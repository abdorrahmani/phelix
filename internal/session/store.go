package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/server"
)

// Dir returns the session store directory under the data dir. It is distinct
// from the data dir's own session.json (the unrelated dashboard auth session).
func Dir() string { return filepath.Join(server.DataDir(), "sessions") }

// sessionPath returns the on-disk record path for id after validating the id,
// so an id can never traverse the filesystem.
func sessionPath(id string) (string, error) {
	if !ValidateID(id) {
		return "", phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"invalid session id %q: expected %s<16 hex chars>", id, IDPrefix)
	}
	return filepath.Join(Dir(), id+".json"), nil
}

// lockFilePath returns the per-session lock-file path (never renamed, so a lock
// held on it protects the record across its atomic replacement).
func lockFilePath(id string) (string, error) {
	if !ValidateID(id) {
		return "", phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"invalid session id %q: expected %s<16 hex chars>", id, IDPrefix)
	}
	return filepath.Join(Dir(), id+".lock"), nil
}

// --- per-session serialization -------------------------------------------
//
// Mutations serialize on a per-session lock so concurrent writers never lose
// an update. Two layers cooperate: an in-process keyed mutex (deterministic
// ordering within one process — the MCP server and race tests) and a BLOCKING
// OS file lock on <id>.lock (cross-process: two CLI invocations, or CLI + MCP
// at once). Reads (Load/List) take neither — a stale read is acceptable, and
// Update re-reads under the lock before it writes.

var (
	locksMu sync.Mutex
	idLocks = map[string]*sync.Mutex{}
)

// inProcLock returns the per-id in-process mutex, locked. The map grows by at
// most one entry per distinct session id touched in this process — negligible
// for the one-shot CLI and, for the long-lived MCP server, bounded by the
// session retention sweep, which drops a pruned session's entry via
// dropInProcLock.
func inProcLock(id string) func() {
	locksMu.Lock()
	m := idLocks[id]
	if m == nil {
		m = &sync.Mutex{}
		idLocks[id] = m
	}
	locksMu.Unlock()
	m.Lock()
	return m.Unlock
}

// dropInProcLock removes the in-process lock for a session that has been
// removed from disk (pruned). It bounds the idLocks map over a long-lived
// host's lifetime. Safe because a pruned session is terminal and never mutated
// again; were it ever touched later, inProcLock simply recreates the entry.
// The cross-process file lock remains the authority across processes.
func dropInProcLock(id string) {
	locksMu.Lock()
	delete(idLocks, id)
	locksMu.Unlock()
}

// lockSession acquires the in-process and cross-process locks for id and
// returns a release func dropping both. Callers MUST defer the release.
func lockSession(id string) (func(), error) {
	release := inProcLock(id)
	path, err := lockFilePath(id)
	if err != nil {
		release()
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		release()
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "create session store directory", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		release()
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "open session lock file", err)
	}
	if err := lockFileBlocking(f); err != nil {
		_ = f.Close()
		release()
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "acquire session lock", err)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
		release()
	}, nil
}

// Decode parses and validates session bytes, failing closed: unparseable
// content is SESSION_CORRUPT; a foreign schema version, malformed stored id or
// unknown status is SESSION_INVALID.
func Decode(data []byte) (*Session, error) {
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeSessionCorrupt, "session content is not a parseable session", err)
	}
	if s.SchemaVersion != SchemaVersion {
		return nil, phelixerr.Newf(phelixerr.CodeSessionInvalid,
			"session schema_version %q, want %q — recreate the session", s.SchemaVersion, SchemaVersion)
	}
	if !ValidateID(s.SessionID) {
		return nil, phelixerr.Newf(phelixerr.CodeSessionInvalid, "session has malformed id %q", s.SessionID)
	}
	if !validStatus(s.Status) {
		return nil, phelixerr.Newf(phelixerr.CodeSessionInvalid, "session has unknown status %q", s.Status)
	}
	return &s, nil
}

// Load reads a session by id and fails closed on any integrity problem:
// missing is NOT_FOUND, unparseable is SESSION_CORRUPT, a foreign schema or
// malformed identity is SESSION_INVALID.
func Load(id string) (*Session, error) {
	path, err := sessionPath(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, phelixerr.Newf(phelixerr.CodeNotFound, "no session %s", id)
	}
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "read session", err)
	}
	return Decode(data)
}

// insert persists a new session exactly once (atomic create-only, like a
// plan). The caller (the service layer) supplies a fully-formed, validated
// Session; the exported entry point is session.Create.
func insert(s *Session) error {
	if s == nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "nil session")
	}
	data, err := encodeSession(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "create session store directory", err)
	}
	final, err := sessionPath(s.SessionID)
	if err != nil {
		return err
	}
	return writeCreateOnly(final, data, s.SessionID)
}

// update performs a locked read-modify-write. It serializes on the session
// lock, re-reads the authoritative record, enforces optimistic concurrency
// (when expectedRev >= 0), applies mutate, bumps the revision and timestamp,
// then atomically replaces the file. mutate runs on the freshly-loaded session
// and may reject the change (e.g. an invalid transition); when it returns an
// error NOTHING is written and the session is left exactly as it was. The
// exported entry points are the session.Checkpoint/Complete/Fail/Cancel verbs.
func update(id string, expectedRev int, mutate func(*Session) error) (*Session, error) {
	release, err := lockSession(id)
	if err != nil {
		return nil, err
	}
	defer release()

	s, err := Load(id)
	if err != nil {
		return nil, err
	}
	if expectedRev >= 0 && s.Rev != expectedRev {
		return nil, phelixerr.Newf(phelixerr.CodeSessionConflict,
			"session %s is at revision %d, not %d; re-read it and retry", id, s.Rev, expectedRev)
	}
	if err := mutate(s); err != nil {
		return nil, err
	}
	s.Rev++
	s.UpdatedAt = time.Now().UnixMilli()
	data, err := encodeSession(s)
	if err != nil {
		return nil, err
	}
	path, err := sessionPath(id)
	if err != nil {
		return nil, err
	}
	if err := atomicReplace(path, data); err != nil {
		return nil, err
	}
	return s, nil
}

// List returns sessions newest-first, optionally filtered by status and app.
// Corrupt or tampered files are skipped and counted rather than failing the
// whole listing — a query path must stay readable; Load still fails closed for
// a specific id. limit <= 0 returns everything; truncated reports whether the
// limit dropped any matching session.
func List(status, app string, limit int) (sessions []*Session, skipped int, truncated bool, err error) {
	entries, rderr := os.ReadDir(Dir())
	if errors.Is(rderr, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if rderr != nil {
		return nil, 0, false, phelixerr.Wrap(phelixerr.CodeFilesystem, "read session store directory", rderr)
	}
	var found []*Session
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" || strings.Contains(e.Name(), ".tmp-") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(Dir(), e.Name()))
		if readErr != nil {
			skipped++
			continue
		}
		s, decErr := Decode(data)
		if decErr != nil {
			skipped++
			continue
		}
		if status != "" && s.Status != status {
			continue
		}
		if app != "" && s.App != app {
			continue
		}
		found = append(found, s)
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].CreatedAt != found[j].CreatedAt {
			return found[i].CreatedAt > found[j].CreatedAt
		}
		return found[i].SessionID > found[j].SessionID
	})
	if limit > 0 && len(found) > limit {
		found = found[:limit]
		truncated = true
	}
	return found, skipped, truncated, nil
}

// DefaultSessionRetention bounds how many TERMINAL sessions are kept on disk.
// Active sessions are never pruned — they are the session store's equivalent of
// the version store's is_current guard, so an in-flight unit of work (and every
// plan/operation it references) survives. Mirrors deploy.PruneVersions and the
// authz decision log's bounded history.
const DefaultSessionRetention = 100

// PruneTerminal deletes the oldest terminal (completed/failed/cancelled)
// sessions beyond max, keeping ALL active sessions and the newest max terminal
// ones. It is best-effort and self-contained: it needs no cross-store
// knowledge because only terminal sessions are ever removed, and a terminal
// session's references are no longer needed by any live work. max <= 0 uses
// DefaultSessionRetention. Each removed session's lock file is removed with it,
// so terminal sessions leave nothing behind.
func PruneTerminal(max int) (int, error) {
	if max <= 0 {
		max = DefaultSessionRetention
	}
	all, _, _, err := List("", "", 0) // newest-first
	if err != nil {
		return 0, err
	}
	kept, pruned := 0, 0
	for _, s := range all { // newest-first
		if !IsTerminal(s.Status) {
			continue // active sessions are always kept and never counted
		}
		if kept < max {
			kept++
			continue
		}
		pruned += removeSessionArtifacts(s.SessionID)
	}
	return pruned, nil
}

// removeSessionArtifacts deletes a session's record and its lock file. It is
// idempotent (a missing file is not an error) and returns 1 when the record
// was removed. A terminal session is never mutated, so there is no writer to
// race; a concurrent reader keeps its open fd on Unix.
func removeSessionArtifacts(id string) int {
	removed := 0
	if path, err := sessionPath(id); err == nil {
		if rmErr := os.Remove(path); rmErr == nil || errors.Is(rmErr, os.ErrNotExist) {
			if rmErr == nil {
				removed = 1
			}
		}
	}
	if lp, err := lockFilePath(id); err == nil {
		_ = os.Remove(lp)
		dropInProcLock(id)
	}
	return removed
}
func encodeSession(s *Session) ([]byte, error) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeSessionInvalid, "encode session", err)
	}
	if len(data) > MaxSessionBytes {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"session %s is %d bytes; the maximum is %d", s.SessionID, len(data), MaxSessionBytes)
	}
	return data, nil
}

// writeCreateOnly writes data to final exactly once, with the plan store's
// discipline: tmp (O_EXCL) + fsync + os.Link (atomic create-only — fails if
// final exists) + directory fsync. A pre-existing id is ALREADY_EXISTS, never
// a silent overwrite.
func writeCreateOnly(final string, data []byte, id string) error {
	if _, err := os.Lstat(final); err == nil {
		return phelixerr.Newf(phelixerr.CodeAlreadyExists, "session %s already exists", id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "stat session", err)
	}
	tmp := fmt.Sprintf("%s.tmp-%d", final, os.Getpid())
	if err := writeSyncClose(tmp, data); err != nil {
		return err
	}
	if err := os.Link(tmp, final); err != nil {
		_ = os.Remove(tmp)
		if errors.Is(err, os.ErrExist) {
			return phelixerr.Newf(phelixerr.CodeAlreadyExists, "session %s already exists", id)
		}
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "persist session", err)
	}
	_ = os.Remove(tmp)
	return fsyncDir(filepath.Dir(final))
}

// atomicReplace overwrites an existing session file (status transitions and
// metadata updates) with the same fsync discipline as creation.
func atomicReplace(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	if err := writeSyncClose(tmp, data); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "replace session", err)
	}
	return fsyncDir(filepath.Dir(path))
}

// writeSyncClose creates tmp exclusively, writes data, fsyncs and closes it.
func writeSyncClose(tmp string, data []byte) error {
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "create session temp file", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "write session", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "sync session", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "close session", err)
	}
	return nil
}

// fsyncDir flushes a directory entry so a crash cannot lose the rename/link.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "open session store directory", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "sync session store directory", err)
	}
	return nil
}
