package secrets

import (
	"context"
	"os"
	"path/filepath"
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
