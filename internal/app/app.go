package app

import (
	"crypto/rand"
	"fmt"
	"os/exec"
	"sync"
)

var (
	apps     = make(map[string]*exec.Cmd)
	appsLock sync.Mutex
)

func GenerateAppID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func StartApplication(id string) {
	appsLock.Lock()
	defer appsLock.Unlock()

	cmd := exec.Command(fmt.Sprintf("./app_%s", id))
	if err := cmd.Start(); err != nil {
		fmt.Println("Start failed:", err)
		return
	}
	apps[id] = cmd
}

func StopApplication(id string) {
	appsLock.Lock()
	defer appsLock.Unlock()

	if cmd, exists := apps[id]; exists {
		cmd.Process.Kill()
		delete(apps, id)
	}
}

func RestartApplication(id string) error {
	appsLock.Lock()
	defer appsLock.Unlock()

	if cmd, exists := apps[id]; exists {
		// Stop the running application
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to stop application %s: %v", id, err)
		}
		delete(apps, id)
	} else {
		fmt.Printf("Application %s was not running, starting it now\n", id)
	}

	// Start it again
	cmd := exec.Command(fmt.Sprintf("./app_%s", id))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to restart application %s: %v", id, err)
	}
	apps[id] = cmd
	return nil
}
