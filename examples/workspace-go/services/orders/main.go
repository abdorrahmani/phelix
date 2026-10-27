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

// authURL is where orders reaches auth. Phelix runs both services on the same
// host, so localhost is correct; a real mesh would take this from config.
const authURL = "http://localhost:8081/token"

// order is one row of the demo order list.
type order struct {
	ID       int    `json:"id"`
	Customer string `json:"customer"`
	Total    int    `json:"total_cents"`
}

// orderList is the body /orders returns.
type orderList struct {
	Orders    []order `json:"orders"`
	AuthToken string  `json:"auth_token,omitempty"`
	AuthError string  `json:"auth_error,omitempty"`
}

// main resolves the port, registers the routes, and serves until the process
// is stopped. A missing PORT falls back to 8082 so the service also runs
// directly with go run.
func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		log.Fatalf("invalid PORT %q: must be an integer in 1-65535", port)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/orders", handleOrders)

	addr := ":" + port
	log.Printf("orders listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// handleHealth reports liveness without touching auth, so a deploy of orders
// never fails because auth is briefly unavailable.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"service":"orders","status":"ok"}`+"\n")
}

// handleOrders returns the demo orders and echoes the token auth handed back,
// which makes the dependency visible with a single curl.
func handleOrders(w http.ResponseWriter, r *http.Request) {
	resp := orderList{Orders: []order{
		{ID: 1, Customer: "ada", Total: 4200},
		{ID: 2, Customer: "grace", Total: 1900},
	}}
	if token, err := fetchAuthToken(); err != nil {
		resp.AuthError = err.Error()
	} else {
		resp.AuthToken = token
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("encode orders: %v", err)
	}
}

// fetchAuthToken asks auth for a token. A failure is reported to the caller
// rather than failing the request, because the order list does not depend on
// the token to be useful.
func fetchAuthToken() (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(authURL + "?for=orders")
	if err != nil {
		return "", fmt.Errorf("auth unreachable: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("auth response unreadable: %w", err)
	}
	return body.Token, nil
}
