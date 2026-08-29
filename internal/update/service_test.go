package update

import (
	"errors"
	"sync"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestSystemdAvailable(t *testing.T) {
	t.Run("systemctl present", func(t *testing.T) {
		runner := &fakeRunner{responses: map[string][]fakeResp{
			"systemctl --version": {{out: "systemd 255"}},
		}}
		if !(Systemd{Runner: runner}).Available() {
			t.Fatal("Available() = false, want true")
		}
	})

	t.Run("systemctl missing", func(t *testing.T) {
		runner := &fakeRunner{} // everything fails
		if (Systemd{Runner: runner}).Available() {
			t.Fatal("Available() = true, want false")
		}
	})

	t.Run("no runner", func(t *testing.T) {
		if (Systemd{}).Available() {
			t.Fatal("Available() with nil runner = true, want false")
		}
	})
}

func TestSystemdUnitExists(t *testing.T) {
	t.Run("unit installed", func(t *testing.T) {
		runner := &fakeRunner{responses: map[string][]fakeResp{
			"systemctl cat phelix.service": {{out: "# /etc/systemd/system/phelix.service"}},
		}}
		if !(Systemd{Runner: runner}).UnitExists() {
			t.Fatal("UnitExists() = false, want true")
		}
	})

	t.Run("unit missing", func(t *testing.T) {
		runner := &fakeRunner{}
		if (Systemd{Runner: runner}).UnitExists() {
			t.Fatal("UnitExists() = true, want false")
		}
	})
}

func TestSystemdIsActive(t *testing.T) {
	cases := []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"active", "active\n", nil, true},
		{"inactive", "inactive\n", errors.New("exit status 3"), false},
		{"failed", "failed\n", errors.New("exit status 3"), false},
		{"query error", "", errors.New("System has not been booted with systemd"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{responses: map[string][]fakeResp{
				"systemctl is-active phelix.service": {{out: tc.out, err: tc.err}},
			}}
			got := (Systemd{Runner: runner}).IsActive()
			if got != tc.want {
				t.Fatalf("IsActive() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSystemdRestart(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		runner := &fakeRunner{responses: map[string][]fakeResp{
			"systemctl restart phelix.service": {{}},
		}}
		if err := (Systemd{Runner: runner}).Restart(false); err != nil {
			t.Fatalf("Restart(false) error: %v", err)
		}
		if !runner.hasCall("systemctl restart phelix.service") {
			t.Fatalf("unexpected calls: %v", runner.calls)
		}
	})

	t.Run("elevated via sudo", func(t *testing.T) {
		runner := &fakeRunner{responses: map[string][]fakeResp{
			"sudo systemctl restart phelix.service": {{}},
		}}
		if err := (Systemd{Runner: runner}).Restart(true); err != nil {
			t.Fatalf("Restart(true) error: %v", err)
		}
		if !runner.hasCall("sudo systemctl restart phelix.service") {
			t.Fatalf("expected sudo-prefixed restart, got %v", runner.calls)
		}
	})

	t.Run("failure", func(t *testing.T) {
		runner := &fakeRunner{responses: map[string][]fakeResp{
			"systemctl restart phelix.service": {{err: errors.New("exit status 1: Job failed")}},
		}}
		err := (Systemd{Runner: runner}).Restart(false)
		if !phelixerr.IsCode(err, phelixerr.CodeUpdateFailed) {
			t.Fatalf("error = %v, want CodeUpdateFailed", err)
		}
	})
}

func TestSystemdWaitActive(t *testing.T) {
	t.Run("becomes active", func(t *testing.T) {
		runner := &fakeRunner{responses: map[string][]fakeResp{
			"systemctl is-active phelix.service": {
				{out: "inactive\n", err: errors.New("exit status 3")},
				{out: "inactive\n", err: errors.New("exit status 3")},
				{out: "active\n"},
			},
		}}
		svc := Systemd{Runner: runner, Poll: time.Millisecond}
		if err := svc.WaitActive(2 * time.Second); err != nil {
			t.Fatalf("WaitActive() error: %v", err)
		}
	})

	t.Run("times out", func(t *testing.T) {
		runner := &fakeRunner{responses: map[string][]fakeResp{
			"systemctl is-active phelix.service": {{out: "inactive\n", err: errors.New("exit status 3")}},
		}}
		svc := Systemd{Runner: runner, Poll: time.Millisecond}
		err := svc.WaitActive(30 * time.Millisecond)
		if !phelixerr.IsCode(err, phelixerr.CodeTimeout) {
			t.Fatalf("error = %v, want CodeTimeout", err)
		}
	})

	t.Run("active immediately", func(t *testing.T) {
		var mu sync.Mutex
		calls := 0
		runner := &fakeRunner{responseFn: func(command string, n int) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			return "active\n", nil
		}}
		svc := Systemd{Runner: runner, Poll: time.Millisecond}
		if err := svc.WaitActive(2 * time.Second); err != nil {
			t.Fatalf("WaitActive() error: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if calls != 1 {
			t.Fatalf("is-active called %d times, want 1 (no polling when already active)", calls)
		}
	})
}
