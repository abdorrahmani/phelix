// Command aware-go-demo demonstrates PATTERN B: docker-compose is the source of
// truth for the app's BUILD (context/args/target), and Phelix builds the image
// FROM that compose service (deploy.docker.build: compose) rather than from a
// standalone Dockerfile it owns. Runtime env still comes from `phelix env`, not
// from the compose service's environment. Standard library only.
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
	// Backing services by docker-compose service name over the shared network.
	// In production these come from encrypted env, NOT the compose file:
	//   phelix env set aware-go-demo DATABASE_URL mysql://user:pass@mysql:3306/app
	//   phelix env set aware-go-demo REDIS_ADDR   redis:6379
	mysqlAddr := envOr("MYSQL_ADDR", "mysql:3306")
	redisAddr := envOr("REDIS_ADDR", "redis:6379")

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "hello from aware-go-demo on port %s\n", port)
	})
	// Liveness only — independent of the backing tier.
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	// Proves both backing services resolve over the shared network.
	mux.HandleFunc("/backends", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "mysql %s -> %s\n", mysqlAddr, reachable(mysqlAddr))
		if pong, err := redisPing(redisAddr); err != nil {
			fmt.Fprintf(w, "redis %s -> unreachable: %v\n", redisAddr, err)
		} else {
			fmt.Fprintf(w, "redis %s -> %s\n", redisAddr, pong)
		}
	})

	addr := ":" + port
	log.Printf("aware-go-demo listening on %s (mysql=%s redis=%s)", addr, mysqlAddr, redisAddr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// reachable dials addr and reports whether the TCP connection opened — enough to
// prove the compose service name resolved over the shared network (no MySQL
// driver needed for the demo).
func reachable(addr string) string {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "unreachable: " + err.Error()
	}
	_ = conn.Close()
	return "reachable"
}

// redisPing sends the one RESP line the demo needs and returns the "+PONG" reply.
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
