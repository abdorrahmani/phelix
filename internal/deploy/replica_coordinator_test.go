package deploy

import (
	"context"
	"testing"
	"time"
)

func TestReplicaCoordinatorExclusive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	var coord ReplicaCoordinator

	release, err := coord.LockApp(context.Background(), "coord-app")
	if err != nil {
		t.Fatalf("LockApp: %v", err)
	}

	_, busy, err := coord.TryLockApp("coord-app")
	if err != nil {
		t.Fatalf("TryLockApp: %v", err)
	}
	if !busy {
		t.Fatal("TryLockApp must report busy while LockApp holds the lock")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := coord.LockApp(ctx, "coord-app"); err == nil {
		t.Fatal("LockApp must fail while the lock is held and the context expires")
	}

	release()
	release2, busy2, err := coord.TryLockApp("coord-app")
	if err != nil || busy2 {
		t.Fatalf("lock must be free after release (busy=%v err=%v)", busy2, err)
	}
	release2()
}

func TestReplicaCoordinatorCancelledContext(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	var coord ReplicaCoordinator

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := coord.LockApp(ctx, "coord-app2"); err == nil {
		t.Fatal("LockApp with a cancelled context must fail")
	}
}
