package main

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serveHTTP runs the MCP server over Streamable HTTP. The token decides the
// security model:
//
//   - token set   → bind 0.0.0.0 and require "Authorization: Bearer <token>".
//   - no token    → bind 127.0.0.1 only, and trust an auth gateway (Caddy
//     forward_auth) in front of us. Since only localhost can connect, the
//     X-Volume-User header Caddy injects can be trusted.
func serveHTTP(server *mcp.Server, addr, token string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid -http address %q: %w", addr, err)
	}

	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		// Behind a reverse proxy the connection is loopback but the Host header
		// is the public name; disable the SDK's DNS-rebinding protection.
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true},
	)

	// MCP at the root, REST alongside it — one binary, two protocols.
	mux := http.NewServeMux()
	mux.Handle("/", mcpHandler)
	mux.HandleFunc("GET /files/", restGet)

	if token == "" {
		bind := "127.0.0.1:" + port
		log.Printf("kbmcp: listening on %s (no token; trusting forward-auth gateway)", bind)
		return http.ListenAndServe(bind, logIdentity(mux))
	}

	bind := "0.0.0.0:" + port
	log.Printf("kbmcp: listening on %s (bearer-token auth)", bind)
	return http.ListenAndServe(bind, requireToken(token, logIdentity(mux)))
}

// logIdentity logs the caller identity injected by the auth gateway, when present.
func logIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := r.Header.Get("X-Volume-User"); user != "" {
			log.Printf("kbmcp: request from user=%q scopes=%q", user, r.Header.Get("X-Volume-Scopes"))
		}
		next.ServeHTTP(w, r)
	})
}

// requireToken wraps next, rejecting any request without the exact bearer token.
func requireToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		// Constant-time compare to avoid leaking the token via timing.
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
