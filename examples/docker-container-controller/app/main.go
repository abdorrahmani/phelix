package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "v1"

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "Phelix example %s\n", version) })
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		for _, key := range []string{"REDIS_ADDRESS", "DATABASE_ADDRESS"} {
			if address := os.Getenv(key); address != "" {
				connection, err := net.DialTimeout("tcp", address, time.Second)
				if err != nil {
					http.Error(w, "infrastructure unavailable", http.StatusServiceUnavailable)
					return
				}
				connection.Close()
			}
		}
		fmt.Fprintln(w, "ready")
	})
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	server := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	log.Printf("example started: %s", version)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
