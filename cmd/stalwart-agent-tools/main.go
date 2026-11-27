package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/8iops/stalwart-agent-tools/internal/broker"
	provider "github.com/8iops/stalwart-agent-tools/internal/runtime"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	brokerURL := required("BINDING_BROKER_URL")
	brokerToken := required("BINDING_BROKER_TOKEN")
	address := os.Getenv("LISTEN_ADDRESS")
	if address == "" {
		address = ":8080"
	}
	resolver, err := broker.New(brokerURL, brokerToken, nil)
	if err != nil {
		log.Fatal(err)
	}
	runtime, err := provider.New(resolver)
	if err != nil {
		log.Fatal(err)
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return runtime.MCP() }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 1 << 20})
	mux := http.NewServeMux()
	mux.Handle("/mcp", authenticate(handler))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Printf("stalwart-agent-tools listening on %s", address)
	log.Fatal(server.ListenAndServe())
}

func authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("Authorization")
		if !strings.HasPrefix(provided, "Bearer ") || len(provided) <= len("Bearer ") || len(provided) > 8199 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(provided, "Bearer ")
		next.ServeHTTP(w, r.WithContext(broker.WithInvocationToken(r.Context(), token)))
	})
}

func required(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}
