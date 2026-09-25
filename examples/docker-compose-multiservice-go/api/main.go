// Command api-demo is one of two independent Go services in the multiservice
// example. Each service is its OWN Phelix app (its own phelix.yaml, its own
// public port); they share one Docker network and one Redis backing service.
// An api → worker call must go through worker's PROXY public port (stable,
// zero-downtime), never worker's container name — Phelix gives managed
// containers no network alias. Standard library only.
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
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		log.Fatalf("invalid PORT %q: must be an integer in 1-65535", port)
	}
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "redis:6379"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "hello from api-demo on port %s\n", port)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/redis", func(w http.ResponseWriter, _ *http.Request) {
		pong, err := redisPing(redisAddr)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "redis %s unreachable: %v\n", redisAddr, err)
			return
		}
		fmt.Fprintf(w, "redis %s -> %s\n", redisAddr, pong)
	})

	addr := ":" + port
	log.Printf("api-demo listening on %s (redis at %s)", addr, redisAddr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func redisPing(addr string) (string, error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("PING\r\n")); err != nil {
		return "", err
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(buf[:n])), nil
}
