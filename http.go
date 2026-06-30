package main

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serveHTTP runs the MCP server over Streamable HTTP on addr, requiring every
// request to present "Authorization: Bearer <token>".
func serveHTTP(server *mcp.Server, addr, token string) error {
	if token == "" {
		return fmt.Errorf("HTTP mode requires a token (set -token or KBMCP_TOKEN)")
	}

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		// Disable the SDK's DNS-rebinding protection: behind a reverse proxy the
		// connection is loopback but the Host header is the public name, which the
		// protection rejects. Auth here is the bearer token, not the Host header.
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true},
	)

	log.Printf("kbmcp: listening on %s (bearer-token auth)", addr)
	return http.ListenAndServe(addr, requireToken(token, handler))
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
