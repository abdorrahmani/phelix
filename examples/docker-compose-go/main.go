// Command compose-go-demo is a minimal Go HTTP service that demonstrates the
// Phelix Docker runtime with SPLIT OWNERSHIP: Phelix builds an image for this
// app and runs each instance as a container attached to a user-defined Docker
// network, while a backing service (Redis) is owned separately by
// docker-compose on that same network. The app reaches Redis by its compose
// DNS name (redis:6379) — proof that deploy.network wired the two tiers
// together. Standard library only (a raw RESP PING, no Redis client).
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

	// The backing service is addressed by its docker-compose SERVICE NAME, which
	// resolves only because Phelix attached this container to the compose network
	// (deploy.network in phelix.yaml). In production this comes from encrypted
	// env: `phelix env set compose-go-demo REDIS_ADDR redis:6379`. The default
	// matches the compose service so the example runs with no env setup.
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "redis:6379"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "hello from compose-go-demo on port %s\n", port)
	})
	// Liveness ONLY — deliberately independent of Redis. Blue-green/rolling gate
	// the traffic switch on this, and a deploy must not fail because a backing
	// service blipped; that is the backing tier's own concern.
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	// Proves the shared-network wiring: dial the backing service by DNS name and
	// report what it answered. 200 with +PONG means deploy.network connected the
	// app tier to the compose backing tier; 503 means it could not be reached.
	mux.HandleFunc("/redis", func(w http.ResponseWriter, _ *http.Request) {
		pong, err := pingRedis(redisAddr)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "redis %s unreachable: %v\n", redisAddr, err)
			return
		}
		fmt.Fprintf(w, "redis %s -> %s\n", redisAddr, pong)
	})

	addr := ":" + port
	log.Printf("compose-go-demo listening on %s (redis at %s)", addr, redisAddr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// pingRedis speaks the one line of the Redis protocol this demo needs: send
// PING, read the "+PONG" reply. Raw TCP keeps the app dependency-free — the
// point is that "redis" resolves over the shared Docker network, not the client.
func pingRedis(addr string) (string, error) {
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
