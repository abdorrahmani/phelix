// Command bluegreen-demo is a minimal Go service for demonstrating a Phelix
// zero-downtime blue-green deploy. Bump the version constant and rebuild to
// watch traffic cut over from the old instance to the new one with no dropped
// requests. Standard library only.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
)

// version is what makes the blue-green switch observable: change it, rebuild,
// and watch `curl` flip from the old value to the new one at cut-over.
const version = "v1"

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		log.Fatalf("invalid PORT %q: must be an integer in 1-65535", port)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "bluegreen-demo %s on port %s\n", version, port)
	})
	// Tier-1 health endpoint: returns 200, so a blue-green candidate is only
	// promoted once it answers here.
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	addr := ":" + port
	log.Printf("bluegreen-demo %s listening on %s", version, addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
