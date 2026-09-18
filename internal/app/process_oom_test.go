package app

import (
	"errors"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/resources"
)

type fakeResourceTracker struct {
	oom    bool
	err    error
	closed bool
}

func (f *fakeResourceTracker) ResourceOOM() (bool, error) { return f.oom, f.err }
func (f *fakeResourceTracker) Close() error               { f.closed = true; return nil }

func TestClassifyResourceExit(t *testing.T) {
	newApp := func() *AppInfo {
		return &AppInfo{
			ID:        "oom-test",
			Name:      "oom-app",
			LogFile:   "/tmp/oom-test.log",
			Resources: resources.Config{Memory: "512Mi"},
		}
	}

	t.Run("cgroup oom kill is a structured resource error", func(t *testing.T) {
		a := newApp()
		tr := &fakeResourceTracker{oom: true}
		a.resourceInstance = tr
		err := a.classifyResourceExit()
		if !phelixerr.IsCode(err, phelixerr.CodeResourceOOM) {
			t.Fatalf("want RESOURCE_OOM, got %v / %s", err, phelixerr.CodeOf(err))
		}
		if !strings.Contains(err.Error(), "512Mi") || !strings.Contains(err.Error(), "oom-app") {
			t.Fatalf("error lost app/limit context: %v", err)
		}
		if !tr.closed || a.resourceInstance != nil {
			t.Fatal("tracking handle not consumed")
		}
	})

	t.Run("non-oom exit keeps existing behavior", func(t *testing.T) {
		a := newApp()
		tr := &fakeResourceTracker{}
		a.resourceInstance = tr
		if err := a.classifyResourceExit(); err != nil {
			t.Fatalf("non-oom exit became an error: %v", err)
		}
		if !tr.closed || a.resourceInstance != nil {
			t.Fatal("tracking handle not consumed")
		}
	})

	t.Run("lost evidence is never a false oom", func(t *testing.T) {
		a := newApp()
		tr := &fakeResourceTracker{err: errors.New("evidence lost")}
		a.resourceInstance = tr
		if err := a.classifyResourceExit(); err != nil {
			t.Fatalf("unreadable evidence became an error: %v", err)
		}
		if !tr.closed {
			t.Fatal("tracking handle not consumed")
		}
	})

	t.Run("unlimited app has nothing to classify", func(t *testing.T) {
		a := newApp()
		if err := a.classifyResourceExit(); err != nil {
			t.Fatalf("nil tracker became an error: %v", err)
		}
	})
}
