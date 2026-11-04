package secrets

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/appid"
	"github.com/abdorrahmani/phelix/internal/env"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

const testAppID = "0f0e1d2c-3b4a-4958-8677-8899aabbccdd"

func testEnv(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PHELIX_DATA_DIR", filepath.Join(root, "data"))
	return root
}

func TestSetCreatesAndVersions(t *testing.T) {
	testEnv(t)
	r1, err := Set(testAppID, "database_url", "postgres://first")
	if err != nil {
		t.Fatalf("first set: %v", err)
	}
	if !r1.Created || r1.Version != 1 || len(r1.Fingerprint) != 12 {
		t.Fatalf("first set result = %+v", r1)
	}
	r2, err := Set(testAppID, "database_url", "postgres://second")
	if err != nil {
		t.Fatalf("second set: %v", err)
	}
	if r2.Created || r2.Version != 2 || r2.Previous != 1 {
		t.Fatalf("second set result = %+v", r2)
	}
	if r2.Fingerprint == r1.Fingerprint {
		t.Fatal("a changed value must change its fingerprint")
	}
	st, err := Open(testAppID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	meta, err := st.Peek("database_url")
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if meta.Version != 2 || meta.Fingerprint != r2.Fingerprint {
		t.Fatalf("meta = %+v", meta)
	}
	got, err := Resolve(context.Background(), testAppID, "database_url")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Value != "postgres://second" || got.Version != 2 || got.Fingerprint != r2.Fingerprint {
		t.Fatalf("resolved = %+v", got)
	}
}

func TestFingerprintIsHMACUnderMasterKey(t *testing.T) {
	testEnv(t)
	res, err := Set(testAppID, "api_key", "hunter2-value")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	master, err := env.GetMasterKey()
	if err != nil {
		t.Fatalf("master key: %v", err)
	}
	sub := hmac.New(sha256.New, master)
	sub.Write([]byte("phelix:secrets:fingerprint:v1"))
	h := hmac.New(sha256.New, sub.Sum(nil))
	h.Write([]byte("hunter2-value"))
	want := hex.EncodeToString(h.Sum(nil))[:12]
	if res.Fingerprint != want {
		t.Fatalf("fingerprint = %s, want %s", res.Fingerprint, want)
	}
}

func TestVersionHistoryTrimsAtFive(t *testing.T) {
	testEnv(t)
	for i := 0; i < 8; i++ {
		if _, err := Set(testAppID, "rotating", strings.Repeat("v", i+1)); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
	}
	st, err := Open(testAppID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	hist, err := st.History("rotating")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != maxVersions {
		t.Fatalf("kept %d versions, want %d", len(hist), maxVersions)
	}
	if hist[0].Version != 4 || hist[len(hist)-1].Version != 8 {
		t.Fatalf("retained range wrong: first=%d last=%d", hist[0].Version, hist[len(hist)-1].Version)
	}
	got, err := Resolve(context.Background(), testAppID, "rotating")
	if err != nil || got.Value != "vvvvvvvv" {
		t.Fatalf("active value = %q err=%v", got.Value, err)
	}
}

func TestValueValidation(t *testing.T) {
	testEnv(t)
	cases := map[string]string{
		"empty":     "",
		"nul byte":  "pass\x00word",
		"oversized": strings.Repeat("a", MaxValueBytes+1),
	}
	for name, value := range cases {
		if _, err := Set(testAppID, "x", value); err == nil {
			t.Fatalf("%s value must be rejected", name)
		}
	}
	if err := ValidateSecretValue("line1\nline2\n"); err != nil {
		t.Fatalf("multi-line values are allowed: %v", err)
	}
	if _, err := Set(testAppID, "Bad_Name", "x"); err == nil {
		t.Fatal("invalid secret names must be rejected")
	}
}

func TestInvalidAppIDRejected(t *testing.T) {
	testEnv(t)
	if _, err := Set("../escape", "x", "y"); err == nil {
		t.Fatal("invalid app id must be rejected")
	}
	if _, err := Open("../escape"); err == nil {
		t.Fatal("open must reject an invalid app id")
	}
}

func TestStorePermissionsAndLockStability(t *testing.T) {
	testEnv(t)
	if _, err := Set(testAppID, "a", "1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	dir := filepath.Join(os.Getenv("PHELIX_DATA_DIR"), "secrets")
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("secrets dir mode = %v err=%v", info.Mode(), err)
	}
	storeFile := filepath.Join(dir, testAppID+".json")
	info, err = os.Stat(storeFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("store mode = %v err=%v", info.Mode(), err)
	}
	lockFile := filepath.Join(dir, testAppID+".lock")
	lockStat1, err := os.Stat(lockFile)
	if err != nil {
		t.Fatalf("lock file must exist after first use: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := Set(testAppID, "a", strings.Repeat("x", i+2)); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
	}
	lockStat2, err := os.Stat(lockFile)
	if err != nil {
		t.Fatalf("lock file must survive saves: %v", err)
	}
	if !os.SameFile(lockStat1, lockStat2) {
		t.Fatal("the lock file must never be replaced by a save")
	}
}

func TestConcurrentWritersLoseNothing(t *testing.T) {
	testEnv(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if _, err := Set(testAppID, "shared", strings.Repeat("x", n+1)); err != nil {
				t.Errorf("concurrent set: %v", err)
			}
		}(i)
	}
	wg.Wait()
	st, err := Open(testAppID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	rec := st.Secrets["shared"]
	if rec.ActiveVersion != 8 {
		t.Fatalf("active version = %d, want 8 (a lost update occurred)", rec.ActiveVersion)
	}
}

func TestTransactionCommitAndDiscard(t *testing.T) {
	t.Run("commit promotes pending bindings idempotently", func(t *testing.T) {
		testEnv(t)
		res, err := Set(testAppID, "db", "database-credential")
		if err != nil {
			t.Fatal(err)
		}
		txn := &PendingTxn{TxnID: NewTxnID(), Bindings: []Binding{{Secret: "db", Env: "DATABASE_URL"}}, Resolved: []ResolvedVersion{{Secret: "db", Version: res.Version, Fingerprint: res.Fingerprint}}}
		if err := BeginTxn(testAppID, txn); err != nil {
			t.Fatalf("begin: %v", err)
		}
		st, _ := Open(testAppID)
		if st.Pending == nil || st.Pending.TxnID != txn.TxnID {
			t.Fatalf("pending = %+v", st.Pending)
		}
		if err := CommitTxn(testAppID); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if err := CommitTxn(testAppID); err != nil {
			t.Fatalf("commit must be idempotent: %v", err)
		}
		st, _ = Open(testAppID)
		if st.Pending != nil {
			t.Fatalf("pending must be cleared, got %+v", st.Pending)
		}
		if len(st.Bindings) != 1 || st.Bindings[0].Env != "DATABASE_URL" {
			t.Fatalf("bindings = %+v", st.Bindings)
		}
	})
	t.Run("discard leaves active bindings untouched", func(t *testing.T) {
		testEnv(t)
		if _, err := Set(testAppID, "old", "old-value"); err != nil {
			t.Fatal(err)
		}
		if err := ReplaceBindings(testAppID, []Binding{{Secret: "old", Env: "OLD"}}); err != nil {
			t.Fatalf("replace: %v", err)
		}
		res, err := Set(testAppID, "new", "new-credential")
		if err != nil {
			t.Fatal(err)
		}
		txn := &PendingTxn{TxnID: NewTxnID(), Bindings: []Binding{{Secret: "new", Env: "NEW"}}, Resolved: []ResolvedVersion{{Secret: "new", Version: res.Version, Fingerprint: res.Fingerprint}}}
		if err := BeginTxn(testAppID, txn); err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := DiscardTxn(testAppID); err != nil {
			t.Fatalf("discard: %v", err)
		}
		if err := DiscardTxn(testAppID); err != nil {
			t.Fatalf("discard must be idempotent: %v", err)
		}
		st, _ := Open(testAppID)
		if st.Pending != nil || len(st.Bindings) != 1 || st.Bindings[0].Env != "OLD" {
			t.Fatalf("state after discard: pending=%+v bindings=%+v", st.Pending, st.Bindings)
		}
	})
	t.Run("invalid bindings are refused", func(t *testing.T) {
		testEnv(t)
		txn := &PendingTxn{TxnID: NewTxnID(), Bindings: []Binding{{Secret: "db", Env: "PORT"}}}
		if err := BeginTxn(testAppID, txn); err == nil {
			t.Fatal("a transaction must not bind reserved names")
		}
		st, _ := Open(testAppID)
		if st.Pending != nil {
			t.Fatal("a refused transaction must not be recorded")
		}
	})
}

func TestRemoveRefusesWhileBound(t *testing.T) {
	testEnv(t)
	if _, err := Set(testAppID, "db", "value"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := ReplaceBindings(testAppID, []Binding{{Secret: "db", Env: "DATABASE_URL"}}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	err := Remove(testAppID, "db")
	if err == nil {
		t.Fatal("remove must refuse while the secret is imported")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("refusal should name the binding, got %v", err)
	}
	if err := Remove(testAppID, "missing"); err == nil || phelixerr.CodeOf(err) != phelixerr.CodeSecretNotFound {
		t.Fatalf("removing a missing secret must report SECRET_NOT_FOUND, got %v", err)
	}
	if err := ReplaceBindings(testAppID, nil); err != nil {
		t.Fatalf("clear bindings: %v", err)
	}
	if err := Remove(testAppID, "db"); err != nil {
		t.Fatalf("remove after unbinding: %v", err)
	}
}

func TestResolveMissingFailsClosed(t *testing.T) {
	testEnv(t)
	_, err := Resolve(context.Background(), testAppID, "ghost")
	if phelixerr.CodeOf(err) != phelixerr.CodeSecretNotFound {
		t.Fatalf("expected SECRET_NOT_FOUND, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Resolve(ctx, testAppID, "ghost"); err == nil {
		t.Fatal("a cancelled context must stop resolution")
	}
}

func TestMultiLineValueRoundTrip(t *testing.T) {
	testEnv(t)
	secret := "-----BEGIN PRIVATE KEY-----\nMIIEvQ\n-----END PRIVATE KEY-----\n"
	if _, err := Set(testAppID, "tls_key", secret); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := Resolve(context.Background(), testAppID, "tls_key")
	if err != nil || got.Value != secret {
		t.Fatalf("round trip changed the value: %q err=%v", got.Value, err)
	}
}

func TestRecordApply(t *testing.T) {
	testEnv(t)
	before := time.Now().UTC()
	if err := RecordApply(testAppID, &ApplyStatus{Restart: ApplyRestartFailed}); err != nil {
		t.Fatalf("record: %v", err)
	}
	st, _ := Open(testAppID)
	if st.LastApply == nil || st.LastApply.Restart != ApplyRestartFailed {
		t.Fatalf("last apply = %+v", st.LastApply)
	}
	if st.LastApply.At.Before(before) {
		t.Fatal("apply status must be stamped")
	}
}

func TestDeferredRotationPreservesLastSuccessfulVersions(t *testing.T) {
	testEnv(t)
	if _, err := Set(testAppID, "db", "first-credential"); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceBindings(testAppID, []Binding{{Secret: "db", Env: "DB"}}); err != nil {
		t.Fatal(err)
	}
	if err := RecordApply(testAppID, &ApplyStatus{Restart: ApplyRestartSucceeded}); err != nil {
		t.Fatal(err)
	}
	if _, err := Set(testAppID, "db", "second-credential"); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{ApplyRestartNotNeeded, ApplyRestartFailed, ApplyRestartSucceeded} {
		if outcome != ApplyRestartNotNeeded {
			if err := RecordApply(testAppID, &ApplyStatus{Restart: outcome}); err != nil {
				t.Fatal(err)
			}
		}
		st, err := Open(testAppID)
		if err != nil {
			t.Fatal(err)
		}
		applied := 1
		if outcome == ApplyRestartSucceeded {
			applied = 2
		}
		if st.LastApply.Restart != outcome || len(st.LastApply.Desired) != 1 || st.LastApply.Desired[0].Version != 2 || len(st.LastApply.Applied) != 1 || st.LastApply.Applied[0].Version != applied {
			t.Fatal("desired and last successful versions became indistinguishable")
		}
	}
}

func TestValidAppIDShape(t *testing.T) {
	if !appid.Valid(testAppID) {
		t.Fatal("a generated UUID-shaped id must validate")
	}
	if appid.Valid("42") {
		t.Fatal("numeric identifiers must not validate")
	}
	for _, bad := range []string{"../../../etc", "..", ".", "a/b", "a\\b", "", "a:b"} {
		if appid.Valid(bad) {
			t.Fatalf("path-hostile id %q must not validate", bad)
		}
	}
}

func TestMissingKeyDoesNotReplaceStoredCredentials(t *testing.T) {
	root := testEnv(t)
	if _, err := Set(testAppID, "db", "preserved-key-value"); err != nil {
		t.Fatal(err)
	}
	path, err := storePath(testAppID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, ".phelix", "master.key")
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(context.Background(), testAppID, "db"); !phelixerr.IsCode(err, phelixerr.CodeEncryption) {
		t.Fatal("missing key must fail resolution", err)
	}
	if _, err := Set(testAppID, "db", "replacement-value"); !phelixerr.IsCode(err, phelixerr.CodeEncryption) {
		t.Fatal("missing key must fail rotation", err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("missing key was regenerated")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("failed rotation changed stored credentials", err)
	}
}

func TestReadRejectsInconsistentSecretMetadata(t *testing.T) {
	for _, damage := range []string{"name", "active", "duplicate", "fingerprint", "pending"} {
		t.Run(damage, func(t *testing.T) {
			testEnv(t)
			if _, err := Set(testAppID, "db", "metadata-value"); err != nil {
				t.Fatal(err)
			}
			st, err := Open(testAppID)
			if err != nil {
				t.Fatal(err)
			}
			record := st.Secrets["db"]
			switch damage {
			case "name":
				record.Name = "other"
			case "active":
				record.ActiveVersion = 2
			case "duplicate":
				record.Versions = append(record.Versions, record.Versions[0])
			case "fingerprint":
				record.Versions[0].Fingerprint = "broken"
			case "pending":
				st.Pending = &PendingTxn{TxnID: NewTxnID(), Bindings: []Binding{{Secret: "db", Env: "DB"}}}
			}
			st.Secrets["db"] = record
			data, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			path, err := storePath(testAppID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(testAppID); err == nil {
				t.Fatal("damaged metadata accepted")
			}
		})
	}
}
