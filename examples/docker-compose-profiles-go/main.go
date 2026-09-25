// Command profiles-go-demo demonstrates PATTERN C: the app is defined in
// docker-compose for LOCAL dev (under a compose profile), but on the SERVER only
// Phelix runs it. The image is built the default way (a Dockerfile); this app is
// never a compose service on the server, so `docker compose up -d` there starts
// only the backing tier. Standard library only.
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	// Phelix injects PORT; the app binds it (same PORT contract as a native app).
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		log.Fatalf("invalid PORT %q: must be an integer in 1-65535", port)
	}
	// Reached by docker-compose service name over the shared network. Overridable
	// with `phelix env set profiles-go-demo REDIS_ADDR ...`; the default matches
	// the compose service.
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "redis:6379"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "hello from profiles-go-demo on port %s\n", port)
	})
	// Liveness only — independent of Redis so a backing blip never fails the
	// blue-green health gate.
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/redis", func(w http.ResponseWriter, _ *http.Request) {
		pong, err := ping(redisAddr, "PING\r\n")
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "redis %s unreachable: %v\n", redisAddr, err)
			return
		}
		fmt.Fprintf(w, "redis %s -> %s\n", redisAddr, pong)
	})

	addr := ":" + port
	log.Printf("profiles-go-demo listening on %s (redis at %s)", addr, redisAddr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// ping opens a TCP connection to addr, optionally writes probe, and returns the
// first line of the reply — enough to prove the name resolved over the shared
// network without pulling in a client library.
func ping(addr, probe string) (string, error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if probe != "" {
		if _, err := conn.Write([]byte(probe)); err != nil {
			return "", err
		}
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(buf[:n])), nil
}
