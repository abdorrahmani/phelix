// Package secrets implements Phelix's local secret store. Each application
// owns one store file that keeps its secrets (encrypted, versioned), the
// bindings that expose those secrets as environment variables, and the
// pending transaction record that keeps those bindings consistent with the
// deployment engine's own journal.
//
// Secrets are separate from the environment variable store on purpose: they
// are resolved when an application starts or restarts, never baked into a
// build, never written into a version snapshot, and never printed. Rolling a
// version back therefore never brings an old credential back.
package secrets

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/abdorrahmani/phelix/internal/appid"
	"github.com/abdorrahmani/phelix/internal/env"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/project"
)

// storeSchemaVersion guards the on-disk store format. Bump only with a
// migration plan; readers refuse foreign versions rather than guessing.
const storeSchemaVersion = 1

// maxVersions is how many recent values a secret keeps. Older versions are
// pruned on write. Since secret values never enter version snapshots,
// pruning a version removes its last copy.
const maxVersions = 5

// MaxValueBytes caps the size of one secret value. Environment variables are
// a poor container for large payloads, and the cap keeps a mistaken file
// upload from filling the store.
const MaxValueBytes = 1 << 20

// Binding maps one secret onto one environment variable name. It is the same
// shape the phelix.yaml secrets section declares, so no conversion is needed
// between the file, the store and the launch path.
type Binding = project.SecretImport

// SecretVersion is one stored value of a secret. The value is encrypted with
// the master key; the fingerprint identifies the plaintext without revealing
// it and is safe to show anywhere.
type SecretVersion struct {
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	Fingerprint string    `json:"fingerprint"`
	EncValue    string    `json:"enc_value"`
}

// SecretRecord is one secret: its identity, its lifecycle metadata and its
// retained versions. The newest version is the active one.
type SecretRecord struct {
	Name          string          `json:"name"`
	ActiveVersion int             `json:"active_version"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	Versions      []SecretVersion `json:"versions"`
}

// ResolvedVersion records which version of which secret a deployment pinned.
// It is durable (it appears in the pending transaction record) but holds no
// value, only the fingerprint.
type ResolvedVersion struct {
	Secret      string `json:"secret"`
	Version     int    `json:"version"`
	Fingerprint string `json:"fingerprint"`
}

// PendingTxn is the write-ahead intent record a deployment writes before it
// touches any instance. It says: this deployment intends to make these
// bindings active, and it pinned these versions. Whether the intent ever
// becomes reality is decided by the deployment engine's own journal, never by
// this record.
type PendingTxn struct {
	TxnID        string            `json:"txn_id"`
	DeploymentID string            `json:"deployment_id,omitempty"`
	Bindings     []Binding         `json:"bindings"`
	Resolved     []ResolvedVersion `json:"resolved"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// ApplyStatus records the outcome of the last `secrets apply` or of a
// value change applied by `secrets set`: the distinction between the desired
// state (what the store holds) and the runtime state (what the running
// instances received).
type ApplyStatus struct {
	At      time.Time `json:"at"`
	Restart string    `json:"restart"`
}

// Apply outcomes recorded in ApplyStatus.Restart.
const (
	ApplyRestartSucceeded = "succeeded"
	ApplyRestartFailed    = "failed"
	ApplyRestartSkipped   = "skipped_not_running"
	ApplyRestartNotNeeded = "not_requested"
)

// Store is the on-disk state of one application's secrets. Read methods on
// it view the snapshot taken when it was opened; every mutation goes through
// the package-level functions, which reload the file under the store lock so
// concurrent processes never lose an update.
type Store struct {
	SchemaVersion int                     `json:"schema_version"`
	AppID         string                  `json:"app_id"`
	Bindings      []Binding               `json:"bindings,omitempty"`
	Pending       *PendingTxn             `json:"pending,omitempty"`
	LastApply     *ApplyStatus            `json:"last_apply,omitempty"`
	Secrets       map[string]SecretRecord `json:"secrets"`
}

// ResolvedSecret is the outcome of resolving one secret: the plaintext value
// (for the caller's use only), the fingerprint (safe to log or show) and the
// version it came from.
type ResolvedSecret struct {
	Value       string
	Fingerprint string
	Version     int
}

// Resolver resolves one secret for one application. The local store
// implements it today; a future external provider implements the same
// contract without changing any call site.
type Resolver interface {
	Resolve(ctx context.Context, appID, name string) (ResolvedSecret, error)
}

// SecretMeta describes a secret without any of its content.
type SecretMeta struct {
	Name        string
	Version     int
	Fingerprint string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	BoundEnvs   []string
}

// VersionMeta describes one retained version of a secret without its value.
type VersionMeta struct {
	Version     int
	Fingerprint string
	CreatedAt   time.Time
}

// dataDir resolves the persistent state directory the same way the rest of
// the runtime does, so the secrets directory moves with PHELIX_DATA_DIR.
func dataDir() string {
	if dir := os.Getenv("PHELIX_DATA_DIR"); dir != "" {
		return dir
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "/var/lib/phelix"
	}
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = os.Getenv("USERPROFILE")
	}
	return filepath.Join(homeDir, ".phelix")
}

// storePath returns the store file for an application after validating the
// identifier, so a hostile app name can never escape the secrets directory.
func storePath(appID string) (string, error) {
	if !appid.Valid(appID) {
		return "", phelixerr.Newf(phelixerr.CodeInvalidArgument, "secrets: invalid application id %q", appID)
	}
	return filepath.Join(dataDir(), "secrets", appID+".json"), nil
}

// lockPath returns the companion lock file for an application. It is created
// once and never replaced or deleted, so every process always locks the same
// file, no matter how many times the store itself is renamed.
func lockPath(appID string) (string, error) {
	if !appid.Valid(appID) {
		return "", phelixerr.Newf(phelixerr.CodeInvalidArgument, "secrets: invalid application id %q", appID)
	}
	return filepath.Join(dataDir(), "secrets", appID+".lock"), nil
}

// openStoreLocked loads the store file. The caller must already hold the
// store lock. A missing file is an empty store: an application with no
// secrets yet is normal, not an error.
func openStoreLocked(path, appID string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		if os.IsNotExist(err) {
			return &Store{SchemaVersion: storeSchemaVersion, AppID: appID, Secrets: map[string]SecretRecord{}}, nil
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "secrets: open store %s", path)
	}
	defer f.Close()
	if err := checkPrivateFile(f); err != nil {
		return nil, err
	}
	var st Store
	dec := json.NewDecoder(f)
	if err := dec.Decode(&st); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "secrets: store %s is unreadable", path)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, phelixerr.New(phelixerr.CodeFilesystem, "secrets: store has trailing data")
	}
	if st.AppID != appID {
		return nil, phelixerr.New(phelixerr.CodeFilesystem, "secrets: store belongs to another application")
	}
	if err := validateBindings(st.Bindings); err != nil {
		return nil, err
	}
	if st.SchemaVersion != storeSchemaVersion {
		return nil, phelixerr.Newf(phelixerr.CodeFilesystem, "secrets: store %s has unsupported schema version %d", path, st.SchemaVersion)
	}
	if st.Secrets == nil {
		st.Secrets = map[string]SecretRecord{}
	}
	st.AppID = appID
	return &st, nil
}

// persistLocked writes the store atomically: temp file created with
// restrictive permissions from the start, fsync, rename, then a directory
// fsync so the rename itself survives a crash. The caller must already hold
// the store lock.
func persistLocked(st *Store, path string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "secrets: create temp store in %s", dir)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "secrets: restrict temp store %s", tmpName)
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		tmp.Close()
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "secrets: encode store", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "secrets: fsync temp store %s", tmpName)
	}
	if err := tmp.Close(); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "secrets: close temp store %s", tmpName)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "secrets: replace store %s", path)
	}
	d, err := os.Open(dir)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "secrets: open store directory", err)
	}
	defer d.Close()
	return phelixerr.Wrap(phelixerr.CodeFilesystem, "secrets: sync store directory", d.Sync())
}

// withStoreLock runs fn while holding the app's store lock. exclusive picks
// LOCK_EX for mutations or LOCK_SH for reads. The lock file itself is never
// renamed, deleted or written, so the lock is stable across saves.
func withStoreLock(appID string, exclusive bool, fn func() error) error {
	lp, err := lockPath(appID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(lp), 0o700); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "secrets: create secrets directory")
	}
	info, err := os.Lstat(filepath.Dir(lp))
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "secrets: inspect directory", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return phelixerr.New(phelixerr.CodeFilesystem, "secrets: directory must have mode 0700 and must not be a symlink")
	}
	f, err := acquireStoreLock(lp, exclusive)
	if err != nil {
		return err
	}
	defer f.Close()
	defer releaseStoreLock(f)
	return fn()
}

// Open loads the store for an application. The returned value is a read-only
// snapshot: safe to hold and inspect, but mutations must use the package
// functions so they serialize against other processes.
func Open(appID string) (*Store, error) {
	path, err := storePath(appID)
	if err != nil {
		return nil, err
	}
	var st *Store
	err = withStoreLock(appID, false, func() error {
		var lerr error
		st, lerr = openStoreLocked(path, appID)
		return lerr
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

// mutate is the single write path. It takes the store lock exclusively,
// reloads the store fresh from disk, applies fn to the reloaded state and
// persists it atomically. Every mutation in this package goes through it, so
// two processes can never both read-then-write the same file.
func mutate(appID string, fn func(st *Store) error) error {
	path, err := storePath(appID)
	if err != nil {
		return err
	}
	return withStoreLock(appID, true, func() error {
		st, err := openStoreLocked(path, appID)
		if err != nil {
			return err
		}
		if err := fn(st); err != nil {
			return err
		}
		return persistLocked(st, path)
	})
}

// fingerprint derives the display fingerprint of a value with HMAC-SHA256
// under a subkey of the master key. Because the subkey is needed, nobody who
// can see a fingerprint can check guesses against it offline; the fingerprint
// answers "did the value change", never "is this value valid".
func fingerprint(master []byte, value string) string {
	sub := hmac.New(sha256.New, master)
	sub.Write([]byte("phelix:secrets:fingerprint:v1"))
	h := hmac.New(sha256.New, sub.Sum(nil))
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// ValidateSecretValue rejects values a secret can never carry: empty values,
// NUL bytes (exec environments cannot pass them) and anything over the size
// cap. The CLI and the store both call it, so a rejected input never reaches
// disk.
func ValidateSecretValue(value string) error {
	if value == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "secrets: value must not be empty\nHint: pass the value on stdin or with --from-file")
	}
	if len(value) > MaxValueBytes {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "secrets: value exceeds the %d byte limit", MaxValueBytes)
	}
	for i := 0; i < len(value); i++ {
		if value[i] == 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "secrets: value must not contain NUL bytes")
		}
	}
	return nil
}

// validateBindings checks a binding set with the same rules the phelix.yaml
// secrets section enforces, so a store can never hold bindings a
// configuration would reject.
func validateBindings(bindings []Binding) error {
	cfg := project.SecretsConfig{Imports: bindings}
	return cfg.Validate()
}

// SetResult reports what one write changed.
type SetResult struct {
	Name        string
	Version     int
	Fingerprint string
	Previous    int
	Created     bool
}

// Set stores a new version of a secret, creating it on first use. The value
// must pass ValidateSecretValue first; the caller is also responsible for
// holding the app's deploy lock so a write never lands in the middle of a
// deployment.
func Set(appID, name, value string) (SetResult, error) {
	res := SetResult{Name: name}
	if _, err := storePath(appID); err != nil {
		return res, err
	}
	if err := validateSecretName(name); err != nil {
		return res, err
	}
	if err := ValidateSecretValue(value); err != nil {
		return res, err
	}
	key, err := env.GetMasterKey()
	if err != nil {
		return res, err
	}
	enc, err := env.EncryptData(value, key)
	if err != nil {
		return res, err
	}
	fp := fingerprint(key, value)
	now := time.Now().UTC()
	err = mutate(appID, func(st *Store) error {
		rec, ok := st.Secrets[name]
		if !ok {
			rec = SecretRecord{Name: name, CreatedAt: now}
			res.Created = true
		}
		res.Previous = rec.ActiveVersion
		v := rec.ActiveVersion + 1
		rec.Versions = append(rec.Versions, SecretVersion{
			Version:     v,
			CreatedAt:   now,
			Fingerprint: fp,
			EncValue:    enc,
		})
		if len(rec.Versions) > maxVersions {
			rec.Versions = rec.Versions[len(rec.Versions)-maxVersions:]
		}
		rec.ActiveVersion = v
		rec.UpdatedAt = now
		st.Secrets[name] = rec
		res.Version = v
		res.Fingerprint = fp
		return nil
	})
	if err != nil {
		return res, err
	}
	return res, nil
}

// Remove deletes a secret and every retained version. It refuses while the
// secret is still referenced by the active bindings or by a pending
// transaction, so removal can never leave the application pointing at a
// secret that no longer exists.
func Remove(appID, name string) error {
	if err := validateSecretName(name); err != nil {
		return err
	}
	return mutate(appID, func(st *Store) error {
		var users []string
		for _, b := range st.Bindings {
			if b.Secret == name {
				users = append(users, b.Env)
			}
		}
		if st.Pending != nil {
			for _, b := range st.Pending.Bindings {
				if b.Secret == name {
					users = append(users, b.Env+" (pending)")
				}
			}
		}
		if len(users) > 0 {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"secrets: %s is still imported as %s\nHint: remove the import from phelix.yaml (and commit or discard any pending change) before deleting the secret",
				name, strings.Join(users, ", "))
		}
		if _, ok := st.Secrets[name]; !ok {
			return phelixerr.Newf(phelixerr.CodeSecretNotFound, "secrets: %s does not exist in this app's store", name)
		}
		delete(st.Secrets, name)
		return nil
	})
}

// ReplaceBindings makes the given bindings the active set, after validating
// them with the phelix.yaml rules. This is the commit step of `secrets apply`
// and of a deployment's binding promotion.
func ReplaceBindings(appID string, bindings []Binding) error {
	if err := validateBindings(bindings); err != nil {
		return err
	}
	return mutate(appID, func(st *Store) error {
		if st.Pending != nil {
			return phelixerr.New(phelixerr.CodeDeployLocked, "secrets: reconcile the pending deployment before changing bindings")
		}
		if err := CheckCollisions(appID, bindings); err != nil {
			return err
		}
		for _, b := range bindings {
			if _, err := st.resolve(b.Secret); err != nil {
				return err
			}
		}
		st.Bindings = append([]Binding(nil), bindings...)
		return nil
	})
}

// BeginTxn records a deployment's write-ahead intent before any instance is
// touched. The record holds bindings and pinned versions, never values. A
// new transaction replaces any leftover: the deployment engine's recovery
// has already resolved (or will resolve) anything a previous one left behind.
func BeginTxn(appID string, txn *PendingTxn) error {
	if txn == nil || txn.TxnID == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "secrets: a transaction needs an id")
	}
	if err := validateBindings(txn.Bindings); err != nil {
		return err
	}
	txn.UpdatedAt = time.Now().UTC()
	return mutate(appID, func(st *Store) error {
		if st.Pending != nil {
			return phelixerr.New(phelixerr.CodeDeployLocked, "secrets: reconcile the pending deployment before starting another")
		}
		st.Pending = txn
		return nil
	})
}

// CommitTxn promotes the pending bindings to the active set and clears the
// transaction. It is idempotent: with no pending record it does nothing. It
// must only be called once the deployment engine's journal shows the
// transaction was accepted; the store never decides this on its own.
func CommitTxn(appID string) error {
	return mutate(appID, func(st *Store) error {
		if st.Pending == nil {
			return nil
		}
		st.Bindings = st.Pending.Bindings
		st.Pending = nil
		return nil
	})
}

// DiscardTxn clears the pending transaction without promoting anything. It
// is idempotent. The active bindings are left exactly as they were.
func DiscardTxn(appID string) error {
	return mutate(appID, func(st *Store) error {
		st.Pending = nil
		return nil
	})
}

// RecordApply stores the outcome of the last apply so the desired state and
// the runtime state stay distinguishable after a failed restart.
func RecordApply(appID string, status *ApplyStatus) error {
	if status == nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "secrets: apply status must not be nil")
	}
	status.At = time.Now().UTC()
	return mutate(appID, func(st *Store) error {
		st.LastApply = status
		return nil
	})
}

// Resolve decrypts the active version of a secret. It implements the
// Resolver contract. A declared but missing secret is an error, never an
// empty value: an application must not start without a variable it declared.
func Resolve(ctx context.Context, appID, name string) (ResolvedSecret, error) {
	if err := ctx.Err(); err != nil {
		return ResolvedSecret{}, phelixerr.Wrap(phelixerr.CodeUnknown, "secrets: resolution cancelled", err)
	}
	st, err := Open(appID)
	if err != nil {
		return ResolvedSecret{}, err
	}
	return st.resolve(name)
}

// resolve reads one value from a store already held by the caller.
func (st *Store) resolve(name string) (ResolvedSecret, error) {
	rec, ok := st.Secrets[name]
	if !ok {
		return ResolvedSecret{}, phelixerr.Newf(phelixerr.CodeSecretNotFound, "secrets: %s does not exist in this app's store", name)
	}
	var ver *SecretVersion
	for i := range rec.Versions {
		if rec.Versions[i].Version == rec.ActiveVersion {
			ver = &rec.Versions[i]
		}
	}
	if ver == nil {
		return ResolvedSecret{}, phelixerr.Newf(phelixerr.CodeFilesystem, "secrets: %s has no active version %d", name, rec.ActiveVersion)
	}
	key, err := env.GetMasterKey()
	if err != nil {
		return ResolvedSecret{}, err
	}
	value, err := env.DecryptData(ver.EncValue, key)
	if err != nil {
		return ResolvedSecret{}, phelixerr.Wrap(phelixerr.CodeEncryption, "secrets: decryption failed; the master key may have changed", err)
	}
	if err := ValidateSecretValue(value); err != nil {
		return ResolvedSecret{}, phelixerr.New(phelixerr.CodeEncryption, "secrets: decrypted value is invalid")
	}
	if fingerprint(key, value) != ver.Fingerprint {
		return ResolvedSecret{}, phelixerr.New(phelixerr.CodeEncryption, "secrets: stored fingerprint does not match the value")
	}
	return ResolvedSecret{Value: value, Fingerprint: ver.Fingerprint, Version: ver.Version}, nil
}

// Peek describes a secret without decrypting it.
func (s *Store) Peek(name string) (SecretMeta, error) {
	rec, ok := s.Secrets[name]
	if !ok {
		return SecretMeta{}, phelixerr.Newf(phelixerr.CodeSecretNotFound, "secrets: %s does not exist in this app's store", name)
	}
	return s.meta(rec), nil
}

// List describes every secret in the store without decrypting anything.
func (s *Store) List() []SecretMeta {
	out := make([]SecretMeta, 0, len(s.Secrets))
	for _, rec := range s.Secrets {
		out = append(out, s.meta(rec))
	}
	return out
}

// History lists the retained versions of one secret, oldest first, with no
// values.
func (s *Store) History(name string) ([]VersionMeta, error) {
	rec, ok := s.Secrets[name]
	if !ok {
		return nil, phelixerr.Newf(phelixerr.CodeSecretNotFound, "secrets: %s does not exist in this app's store", name)
	}
	out := make([]VersionMeta, 0, len(rec.Versions))
	for _, v := range rec.Versions {
		out = append(out, VersionMeta{Version: v.Version, Fingerprint: v.Fingerprint, CreatedAt: v.CreatedAt})
	}
	return out, nil
}

// meta builds the display view of a record, including the environment
// variable names the active bindings route it to.
func (s *Store) meta(rec SecretRecord) SecretMeta {
	meta := SecretMeta{
		Name:        rec.Name,
		Version:     rec.ActiveVersion,
		Fingerprint: "",
		CreatedAt:   rec.CreatedAt,
		UpdatedAt:   rec.UpdatedAt,
	}
	for _, v := range rec.Versions {
		if v.Version == rec.ActiveVersion {
			meta.Fingerprint = v.Fingerprint
		}
	}
	for _, b := range s.Bindings {
		if b.Secret == rec.Name {
			meta.BoundEnvs = append(meta.BoundEnvs, b.Env)
		}
	}
	return meta
}

// NewTxnID mints a transaction identifier with the same shape as the other
// durable record ids.
func NewTxnID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("sec_%016x", time.Now().UnixNano())
	}
	return "sec_" + hex.EncodeToString(b)
}

// txnIDPattern matches the identifiers NewTxnID produces.
var txnIDPattern = regexp.MustCompile(`^sec_[0-9a-f]{16}$`)

// ValidTxnID reports whether id has the shape of a transaction identifier.
func ValidTxnID(id string) bool {
	return txnIDPattern.MatchString(id)
}

// validateSecretName applies the store-side name rule. The phelix.yaml schema
// uses the same pattern, so a name accepted by one is accepted by both.
func validateSecretName(name string) error {
	if !secretNameRe.MatchString(name) {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"secrets: invalid name %q\nHint: use a lowercase name like database_url (letters, digits, hyphen, underscore; must start with a letter)", name)
	}
	return nil
}

// secretNameRe mirrors the pattern in the phelix.yaml schema.
var secretNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// checkPrivateFile refuses files that expose secret records to other users.
func checkPrivateFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "secrets: inspect file", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return phelixerr.New(phelixerr.CodeFilesystem, "secrets: store and lock must be regular files with mode 0600")
	}
	return nil
}

// CheckCollisions refuses live environment entries that also have secret bindings.
func CheckCollisions(appID string, bindings []Binding) error {
	if err := validateBindings(bindings); err != nil {
		return err
	}
	if len(bindings) == 0 {
		return nil
	}
	st, err := env.Instance.LoadEnvStore(appID)
	if err != nil {
		return err
	}
	for _, b := range bindings {
		if _, exists := st.Entries[b.Env]; exists {
			return phelixerr.Newf(phelixerr.CodeConfiguration, "secrets: %s also exists in the live environment store\nHint: remove it with phelix env unset %s %s before deploy or apply", b.Env, appID, b.Env)
		}
	}
	return nil
}
