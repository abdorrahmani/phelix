// Command worker-go is the Phelix worker example: a long-running background
// process with no listener. It ticks simulated jobs, finishes the current job
// on SIGTERM (ack/nack stays the app's responsibility; Phelix owns the signal
// and the wait), and exposes env knobs for demonstrating restart policies and
// health-driven restarts.
//
// CRASH_AFTER_SEC      exit 1 after this many seconds (crash + backoff demo)
// EXIT_CLEAN_AFTER_SEC exit 0 after this many seconds (completed demo)
// JOB_INTERVAL_MS      tick cadence, default 1000
// GRACE_JOBS_MS        how long finishing the current job takes on SIGTERM
//
// `worker-go healthcheck` is the exec health probe: exit 0 unless the marker
// file named by HEALTHCHECK_MARKER exists, so a failed probe (and the
// supervisor's health-driven restart) can be demoed with one touch(1).
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const healthcheckMarkerDefault = "/tmp/worker-go-unhealthy"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}

	started := time.Now()
	jobs := 0
	fmt.Printf("worker-go: started (pid %d), no port, waiting for work\n", os.Getpid())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	crashAfter := durationEnv("CRASH_AFTER_SEC", 0)
	cleanAfter := durationEnv("EXIT_CLEAN_AFTER_SEC", 0)
	interval := durationEnv("JOB_INTERVAL_MS", time.Second)
	grace := durationEnv("GRACE_JOBS_MS", 2*time.Second)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var crashC <-chan time.Time
	if crashAfter > 0 {
		crashC = time.After(crashAfter)
	}

	for {
		select {
		case s := <-sig:
			fmt.Printf("worker-go: %v received, finishing current job before exit\n", s)
			time.Sleep(grace)
			fmt.Printf("worker-go: job %d acknowledged, exiting cleanly\n", jobs)
			return
		case <-ticker.C:
			jobs++
			fmt.Printf("worker-go: processed job %d\n", jobs)
		case <-crashC:
			fmt.Printf("worker-go: simulated crash after %s\n", crashAfter)
			os.Exit(1)
		}

		if cleanAfter > 0 && time.Since(started) >= cleanAfter {
			fmt.Printf("worker-go: simulated clean exit after %s\n", cleanAfter)
			return
		}
	}
}

// runHealthcheck implements the exec probe: healthy unless the marker file
// exists, so `touch /tmp/worker-go-unhealthy` demos a failing replica.
func runHealthcheck() int {
	marker := os.Getenv("HEALTHCHECK_MARKER")
	if marker == "" {
		marker = healthcheckMarkerDefault
	}
	if _, err := os.Stat(marker); err == nil {
		fmt.Printf("worker-go: unhealthy (marker %s present)\n", marker)
		return 1
	}
	fmt.Println("worker-go: healthy")
	return 0
}

func durationEnv(name string, def time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
