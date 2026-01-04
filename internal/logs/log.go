package logs

import (
	"log"
	"os"
	"path/filepath"
)

func RemoveSelfLogs() {
	logFile := filepath.Join(os.Getenv("HOME"), ".gophel", "logs", "gophel.log")

	info, err := os.Stat(logFile)
	if err != nil {
		log.Printf("ERROR: could not stat log file %s: %v", logFile, err)
		return
	}

	if info.Size() > MaxLogSize {
		log.Printf("WARNING: Gophel log file %s too large (%d bytes)", logFile, info.Size())
		err := trimLogFile(logFile)
		if err != nil {
			log.Printf("ERROR: Faild to remove log file %s: %v", logFile, err)
			return
		}
	}
}
