package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

// tokenResponse is the body /token returns to a caller.
type tokenResponse struct {
	Token     string `json:"token"`
	IssuedFor string `json:"issued_for"`
	ExpiresAt string `json:"expires_at"`
}

// main resolves the port, registers the routes, and serves until the process
// is stopped. A missing PORT falls back to 8081 so the service also runs
// directly with go run.
func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		log.Fatalf("invalid PORT %q: must be an integer in 1-65535", port)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/token", handleToken)

	addr := ":" + port
	log.Printf("auth listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// handleHealth reports liveness. Phelix probes this path during a deploy and
// refuses to promote a version that does not answer 2xx.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"service":"auth","status":"ok"}`+"\n")
}

// handleToken returns a placeholder token. It exists so the dependent
// services have a real call to make when they start.
func handleToken(w http.ResponseWriter, r *http.Request) {
	issued := time.Now().UTC()
	resp := tokenResponse{
		Token:     fmt.Sprintf("demo-%d", issued.UnixNano()),
		IssuedFor: r.URL.Query().Get("for"),
		ExpiresAt: issued.Add(time.Hour).Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("encode token: %v", err)
	}
}
