package deploy

import (
	"context"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// ReplicaCoordinator serializes every component that decides what a worker's
// replicas should be doing: the deploy engine, phelix scale, manual
// start/stop/restart, the initial build, and the supervisor. It is a thin
// naming layer over the per-app deploy lock (flock on deploy.lock, dropped
// automatically when a holder dies), so all mutators exclude each other the
// same way two concurrent deploys already do.
//
// Contract: whoever holds LockApp owns every replica decision for that app
// until release. The supervisor try-locks each reconcile pass and skips the
// pass when the lock is busy; a deploy therefore never races a mid-flight
// restart decision.
type ReplicaCoordinator struct{}

// LockApp takes the app's replica lifecycle lock and returns release, which
// must be called on every path (success or failure). ctx only bounds the
// wait: the lock is still exclusive once acquired.
func (ReplicaCoordinator) LockApp(ctx context.Context, appName string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeDeployLocked, "replica coordinator: cancelled before lock", err)
	}
	done := make(chan struct{})
	var (
		release func()
		err     error
	)
	go func() {
		release, err = AcquireDeployLock(appName, "replicas")
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		go func() {
			<-done
			if release != nil {
				release()
			}
		}()
		return nil, phelixerr.Wrap(phelixerr.CodeDeployLocked, "replica coordinator: timed out waiting for the app lock", ctx.Err())
	}
	if err != nil {
		return nil, err
	}
	return release, nil
}

// TryLockApp takes the app lock only when it is free, for the supervisor's
// reconcile passes. busy is true (and release nil) when another component
// holds the lock: the caller skips its pass instead of queueing behind a
// multi-minute deploy.
func (ReplicaCoordinator) TryLockApp(appName string) (release func(), busy bool, err error) {
	release, err = AcquireDeployLock(appName, "supervisor")
	if err == nil {
		return release, false, nil
	}
	if phelixerr.CodeOf(err) == phelixerr.CodeDeployLocked {
		return nil, true, nil
	}
	return nil, false, err
}

// LockTimeout bounds how long a mutator waits for the app lock before giving
// up. Deploys can hold it for minutes, so callers surface the current holder
// (operation + start time) in the error.
const LockTimeout = 30 * time.Second
