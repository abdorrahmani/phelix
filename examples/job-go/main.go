package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func main() {
	message := flag.String("message", "job ran", "line appended to the output file")
	output := flag.String("output", "job-runs.log", "file that receives one line per run")
	fail := flag.Bool("fail", false, "exit 1 to demonstrate a failed execution")
	flag.Parse()

	line := fmt.Sprintf("%s %s\n", time.Now().UTC().Format(time.RFC3339), *message)
	if err := os.MkdirAll(filepath.Dir(*output), 0o755); err != nil && filepath.Dir(*output) != "." {
		fmt.Fprintf(os.Stderr, "create output directory: %v\n", err)
		os.Exit(1)
	}
	f, err := os.OpenFile(*output, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", *output, err)
		os.Exit(1)
	}
	defer f.Close()

	if _, err := f.WriteString(line); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *output, err)
		os.Exit(1)
	}
	fmt.Printf("%s", line)
	if *fail {
		fmt.Fprintln(os.Stderr, "requested failure")
		os.Exit(1)
	}
}
