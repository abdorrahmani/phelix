// Command simple-go is a minimal HTTP service that binds the port Phelix
// provides in the PORT environment variable — the smallest app that satisfies
// the Phelix PORT contract. Standard library only.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
)

func main() {
	// Phelix injects PORT for each managed instance. Fall back to 8080 only so
	// the example is also runnable directly with `go run .`.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Fail fast with a useful message if PORT is not a usable TCP port,
	// instead of letting ListenAndServe emit an opaque error.
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		log.Fatalf("invalid PORT %q: must be an integer in 1-65535", port)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "hello from simple-go on port %s\n", port)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	addr := ":" + port
	log.Printf("simple-go listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
