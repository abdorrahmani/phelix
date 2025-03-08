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
