package deploy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

var longHolder = DeployLock{Operation: "rollback-classic", PID: 48120}
var shortHolder = DeployLock{Operation: "build", PID: 512}

func TestWriteHolder_ShorterHeaderReplacesLonger(t *testing.T) {
	resetHome(t)
	path, err := lockPath("lockheader")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	h := &lockHandle{f: f}
	h.writeHolder(longHolder)
	h.writeHolder(shortHolder)

	got := readLockHolder(path)
	if got == nil {
		t.Fatal("a short header written over a long one must still parse")
	}
	if got.Operation != shortHolder.Operation || got.PID != shortHolder.PID {
		t.Fatalf("holder = %+v, want %+v", got, shortHolder)
	}
}

func TestMarshalLockFile_ShorterDocumentReplacesLonger(t *testing.T) {
	resetHome(t)

	// MarshalLockFile writes the long, indented form.
	if err := MarshalLockFile("lockheader2", longHolder); err != nil {
		t.Fatal(err)
	}
	path, err := lockPath("lockheader2")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatalf("indented header is not valid JSON: %q", data)
	}

	if err := MarshalLockFile("lockheader2", shortHolder); err != nil {
		t.Fatal(err)
	}

	got := readLockHolder(path)
	if got == nil || got.Operation != shortHolder.Operation {
		t.Fatalf("holder after rewrite = %+v, want %+v", got, shortHolder)
	}
}

func TestAcquireDeployLock_ContentionErrorNamesTheHolder(t *testing.T) {
	resetHome(t)
	const app = "lockheader3"

	path, err := lockPath(app)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}

	(&lockHandle{f: f}).writeHolder(longHolder)
	_ = f.Close()

	release, err := AcquireDeployLock(app, "build")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	_, err = AcquireDeployLock(app, "rollback")
	if err == nil {
		t.Fatal("a second acquire must be refused while the first holds the lock")
	}

	if !phelixerr.IsCode(err, phelixerr.CodeDeployLocked) {
		t.Fatalf("code = %s, want DEPLOY_LOCKED", phelixerr.CodeOf(err))
	}

	if !strings.Contains(err.Error(), `"build"`) {
		t.Fatalf("contention error must name the holding operation, got: %v", err)
	}
}
