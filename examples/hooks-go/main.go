package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
)

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
		fmt.Fprintf(w, "hello from hooks-go on port %s\n", port)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ready")
	})

	addr := ":" + port
	log.Printf("hooks-go listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
