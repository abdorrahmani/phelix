package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestReconcileOrphanContainers_RemovesOnlyUnknown(t *testing.T) {
	// docker ps lists three managed containers; state knows only "keep1".
	// The other two are orphans and must be force-removed.
	var rmCalls []string
	run := func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "ps":
			return "keep1\norphan1\norphan2\n", nil
		case "rm":
			rmCalls = append(rmCalls, args[len(args)-1])
			return "", nil
		default:
			return "", nil
		}
	}
	known := map[string]bool{"keep1": true}
	removed, err := reconcileOrphanContainers(context.Background(), known, run)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("expected 2 removed, got %v", removed)
	}
	// keep1 must never be removed.
	for _, id := range rmCalls {
		if id == "keep1" {
			t.Fatal("a state-tracked container was force-removed")
		}
	}
	// The rm must be force (-f), since an orphan may still be running.
	if len(rmCalls) != 2 {
		t.Fatalf("expected 2 rm calls, got %v", rmCalls)
	}
}

func TestReconcileOrphanContainers_ForceFlag(t *testing.T) {
	var sawForce bool
	run := func(_ context.Context, args ...string) (string, error) {
		if args[0] == "ps" {
			return "orphan\n", nil
		}
		if args[0] == "rm" {
			sawForce = strings.Contains(strings.Join(args, " "), "-f")
		}
		return "", nil
	}
	if _, err := reconcileOrphanContainers(context.Background(), nil, run); err != nil {
		t.Fatal(err)
	}
	if !sawForce {
		t.Fatal("orphan removal must use rm -f")
	}
}

func TestReconcileOrphanContainers_NothingToDo(t *testing.T) {
	run := func(_ context.Context, args ...string) (string, error) {
		if args[0] == "ps" {
			return "\n", nil // no managed containers
		}
		t.Fatalf("rm must not be called when there are no containers: %v", args)
		return "", nil
	}
	removed, err := reconcileOrphanContainers(context.Background(), nil, run)
	if err != nil || len(removed) != 0 {
		t.Fatalf("expected clean no-op, got removed=%v err=%v", removed, err)
	}
}

func TestReconcileOrphanContainers_DaemonDownIsError(t *testing.T) {
	run := func(_ context.Context, args ...string) (string, error) {
		return "", errors.New("Cannot connect to the Docker daemon")
	}
	if _, err := reconcileOrphanContainers(context.Background(), nil, run); err == nil {
		t.Fatal("a docker ps failure must surface as an error (logged by caller, not fatal)")
	}
}

func TestReconcileOrphanContainers_PerContainerFailureSkips(t *testing.T) {
	// One rm fails; the other must still be attempted and reported.
	run := func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "ps":
			return "bad\ngood\n", nil
		case "rm":
			if args[len(args)-1] == "bad" {
				return "", errors.New("device or resource busy")
			}
			return "", nil
		}
		return "", nil
	}
	removed, err := reconcileOrphanContainers(context.Background(), nil, run)
	if err != nil {
		t.Fatalf("a single rm failure must not fail the whole reconcile: %v", err)
	}
	if len(removed) != 1 || removed[0] != "good" {
		t.Fatalf("expected only 'good' removed, got %v", removed)
	}
}

func TestKnownContainerIDs_CollectsSlotsAndReplicas(t *testing.T) {
	state := &DeployState{
		Slots: map[string]*Instance{
			"blue":  {ContainerID: "c-blue"},
			"green": {ContainerID: ""}, // no container — skipped
		},
		Replicas: map[string]*Instance{
			"0": {ContainerID: "c-r0"},
			"1": {ContainerID: "c-r1"},
		},
	}
	ids := KnownContainerIDs(state)
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	for _, want := range []string{"c-blue", "c-r0", "c-r1"} {
		if !got[want] {
			t.Errorf("missing %q in %v", want, ids)
		}
	}
	if got[""] {
		t.Error("empty container id must not be collected")
	}
}
