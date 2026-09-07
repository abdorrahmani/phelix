package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/fatih/color"
)

// verifyProgressPrinter returns an OnTick callback that lazily prints the
// verification header (once, before the first sweep result) and then one
// progress line per observation sweep, following the existing ✓/✗ convention.
func verifyProgressPrinter() func(elapsed time.Duration, err error) {
	header := false
	return func(elapsed time.Duration, err error) {
		if !header {
			fmt.Printf("%s Verifying rollback stability...\n", color.BlueString("→"))
			header = true
		}
		line := deploy.FormatVerifyProgress(elapsed, err)
		if err != nil {
			fmt.Println(color.RedString(line))
			return
		}
		fmt.Println(color.GreenString(line))
	}
}

// installVerifyInterruptHandler cancels ctx on the first SIGINT/SIGTERM. The
// returned restore unwires the handler; both the signal channel and the
// watcher goroutine are released when verification ends, so nothing leaks.
func installVerifyInterruptHandler(ctx context.Context, cancel context.CancelFunc) (restore func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sigCh)
		close(done)
	}
}

// verifyContext returns a context cancelled by SIGINT/SIGTERM. Only installed
// when verification is requested: rollbacks without --verify keep their
// existing signal behavior untouched.
func verifyContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	restore := installVerifyInterruptHandler(ctx, cancel)
	return ctx, restore
}

// runStabilityVerification drives one verification window for a classic
// rollback (no DeployState — the probe target is the freshly started app
// process on its port). It prints progress, handles Ctrl+C (cancel ≠ rollback
// failure: execution already committed), and returns the history record plus
// the error to surface for a non-passed outcome.
func runStabilityVerification(appInfo *app.AppInfo, duration time.Duration) (*deploy.RollbackVerification, error) {
	// Re-read the app record so the PID/Port reflect the instance
	// StartApplication just launched, not the pre-rollback snapshot.
	if err := app.Manager.LoadState(); err == nil {
		if fresh, ferr := GetAppInfo(appInfo.ID); ferr == nil && fresh != nil {
			appInfo = fresh
		}
	}
	port := appInfo.Port
	if port == 0 {
		port = defaultPort
	}

	ctx, restore := verifyContext()
	defer restore()

	err := deploy.VerifyRollbackStability(ctx, deploy.VerificationOptions{
		AppID:    appInfo.ID,
		Duration: duration,
		Targets: []deploy.VerifyTarget{{
			Label:    "classic instance",
			HostPort: fmt.Sprintf("127.0.0.1:%d", port),
			PID:      appInfo.PID,
		}},
		Logger: &colorLogger{},
		OnTick: verifyProgressPrinter(),
	})
	if err == nil {
		fmt.Printf("%s Rollback remained healthy for %s\n", color.GreenString("✓"), duration)
		return &deploy.RollbackVerification{
			Requested: true,
			Duration:  duration.String(),
			Status:    deploy.RollbackVerifyPassed,
			At:        time.Now(),
		}, nil
	}
	if ctx.Err() != nil {
		// Interrupted: execution succeeded, observation stopped early. Never
		// reclassify this as a rollback execution failure.
		fmt.Printf("%s Rollback verification cancelled; rollback remains active\n", color.YellowString("⚠"))
		return &deploy.RollbackVerification{
			Requested: true,
			Duration:  duration.String(),
			Status:    deploy.RollbackVerifyCancelled,
			At:        time.Now(),
		}, phelixerr.Newf(phelixerr.CodeRollbackVerifyFailed,
			"rollback verification cancelled; rollback remains active")
	}
	fmt.Printf("%s Rollback verification failed\n", color.RedString("✗"))
	fmt.Printf("\nRollback execution: SUCCESS\nVerification:       FAILED\n")
	return &deploy.RollbackVerification{
		Requested: true,
		Duration:  duration.String(),
		Status:    deploy.RollbackVerifyFailed,
		Error:     err.Error(),
		At:        time.Now(),
	}, phelixerr.Wrapf(phelixerr.CodeRollbackVerifyFailed, err,
		"the application did not remain healthy during the %s verification window", duration)
}
