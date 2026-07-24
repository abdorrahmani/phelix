package health

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// GlobalDaemon manages a singleton health check daemon instance
type GlobalDaemon struct {
	daemon            *Daemon
	reporter          HealthReporter
	mu                sync.RWMutex
	started           bool
	initAttempts      int
	maxInitAttempts   int
	initRetryInterval time.Duration
}

var globalDaemon *GlobalDaemon
var globalDaemonOnce sync.Once

// InitGlobalDaemon initializes the global health daemon singleton
func InitGlobalDaemon() *GlobalDaemon {
	globalDaemonOnce.Do(func() {
		globalDaemon = &GlobalDaemon{
			maxInitAttempts:   30,
			initRetryInterval: 1 * time.Second,
		}
	})
	return globalDaemon
}

// GetGlobalDaemon returns the global daemon instance
func GetGlobalDaemon() *GlobalDaemon {
	if globalDaemon == nil {
		panic("GlobalDaemon not initialized")
	}
	return globalDaemon
}

// SetReporter sets the health reporter. Can be called before or after Start().
func (gd *GlobalDaemon) SetReporter(r HealthReporter) {
	gd.mu.Lock()
	defer gd.mu.Unlock()
	gd.reporter = r
	if gd.daemon != nil {
		gd.daemon.SetReporter(r)
	}
}

// Start initializes and starts the daemon with retries
func (gd *GlobalDaemon) Start() error {
	gd.mu.Lock()
	defer gd.mu.Unlock()

	if gd.started && gd.daemon != nil {
		return nil
	}

	var err error
	gd.initAttempts = 0

	for gd.initAttempts < gd.maxInitAttempts {
		gd.initAttempts++

		if gd.daemon, err = InitDaemon(); err != nil {
			log.Printf("[Health] Failed to initialize daemon (attempt %d/%d): %v", gd.initAttempts, gd.maxInitAttempts, err)
			time.Sleep(gd.initRetryInterval)
			continue
		}

		// Apply reporter to the newly created daemon
		if gd.reporter != nil {
			gd.daemon.SetReporter(gd.reporter)
		}

		if err := gd.daemon.Start(); err != nil {
			log.Printf("[Health] Failed to start daemon (attempt %d/%d): %v", gd.initAttempts, gd.maxInitAttempts, err)
			gd.daemon = nil
			time.Sleep(gd.initRetryInterval)
			continue
		}

		gd.started = true
		log.Println("[Health] Global daemon started successfully")
		return nil
	}

	return fmt.Errorf("failed to start health daemon after %d attempts: %w", gd.maxInitAttempts, err)
}

// GetDaemon returns the underlying daemon (blocking until it's initialized on first call)
func (gd *GlobalDaemon) GetDaemon() (*Daemon, error) {
	gd.mu.RLock()
	if gd.daemon != nil {
		defer gd.mu.RUnlock()
		return gd.daemon, nil
	}
	gd.mu.RUnlock()

	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		gd.mu.RLock()
		if gd.daemon != nil {
			defer gd.mu.RUnlock()
			return gd.daemon, nil
		}
		gd.mu.RUnlock()
	}

	return nil, fmt.Errorf("daemon not initialized within 5 seconds")
}

// Stop stops the daemon
func (gd *GlobalDaemon) Stop() error {
	gd.mu.Lock()
	defer gd.mu.Unlock()

	if gd.daemon == nil {
		return nil
	}

	if err := gd.daemon.Stop(); err != nil {
		return err
	}

	gd.started = false
	return nil
}

// IsRunning returns whether the daemon is running
func (gd *GlobalDaemon) IsRunning() bool {
	gd.mu.RLock()
	defer gd.mu.RUnlock()
	return gd.started && gd.daemon != nil && gd.daemon.IsRunning()
}
