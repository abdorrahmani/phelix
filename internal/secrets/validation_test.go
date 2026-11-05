package secrets

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/abdorrahmani/phelix/internal/env"
)

func TestBindingsValidateBeforeWrite(t *testing.T) {
	testEnv(t)
	if _, err := Set(testAppID, "db", "credential"); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceBindings(testAppID, []Binding{{Secret: "db", Env: "DB"}}); err != nil {
		t.Fatal(err)
	}
	path, _ := storePath(testAppID)
	before, _ := os.ReadFile(path)
	for _, bindings := range [][]Binding{
		{{Secret: "missing", Env: "DB"}},
		{{Secret: " db", Env: "DB"}},
		{{Secret: "db", Env: "DB "}},
		{{Secret: "db", Env: "PORT"}},
	} {
		if err := ReplaceBindings(testAppID, bindings); err == nil {
			t.Fatal("invalid binding was accepted")
		}
		after, _ := os.ReadFile(path)
		if string(after) != string(before) {
			t.Fatal("rejected binding changed the store")
		}
	}
}

func TestPendingCannotBeOverwritten(t *testing.T) {
	testEnv(t)
	first := &PendingTxn{TxnID: NewTxnID()}
	if err := BeginTxn(testAppID, first); err != nil {
		t.Fatal(err)
	}
	if err := BeginTxn(testAppID, &PendingTxn{TxnID: NewTxnID()}); err == nil {
		t.Fatal("pending transaction was overwritten")
	}
	st, err := Open(testAppID)
	if err != nil || st.Pending.TxnID != first.TxnID {
		t.Fatal("pending transaction changed")
	}
}

func TestUnsafePermissionsFailClosed(t *testing.T) {
	for _, name := range []string{"directory", "store", "lock"} {
		t.Run(name, func(t *testing.T) {
			testEnv(t)
			if _, err := Set(testAppID, "db", "credential"); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(os.Getenv("PHELIX_DATA_DIR"), "secrets")
			if name == "store" {
				path = filepath.Join(path, testAppID+".json")
			} else if name == "lock" {
				path = filepath.Join(path, testAppID+".lock")
			}
			if err := os.Chmod(path, 0o777); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(testAppID); err == nil {
				t.Fatal("unsafe permissions accepted")
			}
		})
	}
}

func TestLiveCollisionBlocksApplyAndPin(t *testing.T) {
	testEnv(t)
	if _, err := Set(testAppID, "db", "credential"); err != nil {
		t.Fatal(err)
	}
	if err := env.Instance.SetEnv(testAppID, "DB", "plain"); err != nil {
		t.Fatal(err)
	}
	bindings := []Binding{{Secret: "db", Env: "DB"}}
	if err := ReplaceBindings(testAppID, bindings); err == nil {
		t.Fatal("apply accepted live collision")
	}
	if _, err := PinBindings(context.Background(), testAppID, bindings); err == nil {
		t.Fatal("deployment accepted live collision")
	}
}

func TestIntentSerializesAgainstConcurrentRotation(t *testing.T) {
	testEnv(t)
	for i := 0; i < 20; i++ {
		if _, err := Set(testAppID, "db", "before-race"); err != nil {
			t.Fatal(err)
		}
		set, err := PinBindings(context.Background(), testAppID, []Binding{{Secret: "db", Env: "DB"}})
		if err != nil {
			t.Fatal(err)
		}
		var beginErr, setErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			beginErr = BeginTxn(testAppID, &PendingTxn{TxnID: NewTxnID(), Bindings: set.Bindings, Resolved: set.Resolved})
		}()
		go func() {
			defer wg.Done()
			_, setErr = Set(testAppID, "db", "after-race")
		}()
		wg.Wait()
		set.Close()
		if (beginErr == nil) == (setErr == nil) {
			t.Fatalf("exactly one operation must succeed: intent=%v rotation=%v", beginErr, setErr)
		}
		if err := DiscardTxn(testAppID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIntentRejectsMissingAndInconsistentVersions(t *testing.T) {
	testEnv(t)
	res, err := Set(testAppID, "db", "test-credential")
	if err != nil {
		t.Fatal(err)
	}
	for _, versions := range [][]ResolvedVersion{
		nil,
		{{Secret: "db", Version: res.Version + 1, Fingerprint: res.Fingerprint}},
		{{Secret: "db", Version: res.Version, Fingerprint: "bad"}},
		{{Secret: "other", Version: res.Version, Fingerprint: res.Fingerprint}},
	} {
		if err := BeginTxn(testAppID, &PendingTxn{TxnID: NewTxnID(), Bindings: []Binding{{Secret: "db", Env: "DB"}}, Resolved: versions}); err == nil {
			t.Fatal("invalid intent accepted")
		}
		st, err := Open(testAppID)
		if err != nil || st.Pending != nil {
			t.Fatal("rejected intent changed store", err)
		}
	}
}

func TestAbsentStoreReadCreatesNothing(t *testing.T) {
	testEnv(t)
	if _, err := Open(testAppID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("PHELIX_DATA_DIR"), "secrets")); !os.IsNotExist(err) {
		t.Fatal("read created secret files for an app without secrets")
	}
}
