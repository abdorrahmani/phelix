package Gophel

import (
	"fmt"
	"os/exec"
)

func main() {
	// Check and install Go if not present
	checkGoInstallation()
}

func checkGoInstallation() {
	if _, err := exec.LookPath("go"); err != nil {
		fmt.Println("Go not found. Installing latest version...")
		exec.Command("sudo", "apt", "update").Run()
		exec.Command("sudo", "apt", "install", "golang-go").Run()
	}
}
