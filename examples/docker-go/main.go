// Command docker-go-demo is a minimal Go HTTP service used to demonstrate the
// Phelix Docker runtime: Phelix builds an image for this app and runs each
// instance as a container, injecting PORT exactly as it does for a native
// process. Standard library only.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
)

func main() {
	// Phelix injects PORT into the container for each instance. The app must
	// bind it; nothing is EXPOSEd or published in the Dockerfile — Phelix
	// publishes the container port to a private 127.0.0.1 host port itself.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		log.Fatalf("invalid PORT %q: must be an integer in 1-65535", port)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "hello from docker-go-demo on port %s\n", port)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	addr := ":" + port
	log.Printf("docker-go-demo listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
