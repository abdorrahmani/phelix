package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// upstreams maps a gateway prefix to the service that answers it.
var upstreams = map[string]string{
	"/auth/":   "http://localhost:8081/",
	"/orders/": "http://localhost:8082/",
}

// main resolves the port, wires one reverse proxy per upstream, and serves
// until the process is stopped. A missing PORT falls back to 8080 so the
// gateway also runs directly with go run.
func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		log.Fatalf("invalid PORT %q: must be an integer in 1-65535", port)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/", handleRoot)
	for prefix, target := range upstreams {
		proxy, err := newProxy(prefix, target)
		if err != nil {
			log.Fatalf("upstream %s: %v", target, err)
		}
		mux.Handle(prefix, proxy)
	}

	addr := ":" + port
	log.Printf("gateway listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// handleHealth reports gateway liveness. It never proxies, so a failing
// upstream cannot fail a gateway deploy.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"service":"gateway","status":"ok"}`+"\n")
}

// handleRoot lists the routes the gateway exposes, which makes a bare curl of
// the public port useful.
func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"service":"gateway","routes":["/auth/token","/orders/orders","/health"]}`+"\n")
}

// newProxy builds the reverse proxy for one upstream and strips the gateway
// prefix, so /auth/token reaches the service as /token. A 30 second response
// header timeout keeps a stuck upstream from holding a connection forever.
func newProxy(prefix, target string) (*httputil.ReverseProxy, error) {
	targetURL, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	inner := proxy.Director
	proxy.Director = func(r *http.Request) {
		inner(r)
		r.URL.Path = strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(prefix, "/"))
	}
	proxy.Transport = &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return proxy, nil
}
