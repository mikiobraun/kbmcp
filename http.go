package main

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serveHTTP runs the MCP server over Streamable HTTP behind an auth gateway. It
// binds the address passed to -http (loopback by default, when only a port is
// given) and always requires a shared bearer token ("Authorization: Bearer
// <token>"): the gateway (Caddy) is the only ingress and injects that token,
// which proves the caller is the gateway — so the X-Volume-User /
// X-Volume-Scopes headers it also injects can be trusted. A same-host gateway
// reaches loopback; a gateway on another host (e.g. a VM over Tailscale) needs a
// routable bind like the Tailscale IP — pass it as -http 100.x.y.z:8070. For an
// unauthenticated local server, use stdio instead.
func serveHTTP(server *mcp.Server, addr, token string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid -http address %q: %w", addr, err)
	}
	if token == "" {
		return fmt.Errorf("HTTP mode requires a bearer token: set $KBMCP_TOKEN or pass -token (use stdio for an unauthenticated local server)")
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
	mux.HandleFunc("PUT /files/", restPut)
	mux.HandleFunc("GET /history", restHistory)
	mux.HandleFunc("GET /search", restSearch)

	// Default to loopback when only a port was given; the gateway is the sole
	// ingress and the token proves the caller is the gateway. Bind a routable
	// address (e.g. the Tailscale IP) only when the gateway is on another host.
	if host == "" {
		host = "127.0.0.1"
	}
	bind := net.JoinHostPort(host, port)
	log.Printf("kbmcp: listening on %s (bearer-token auth, behind gateway)", bind)
	return http.ListenAndServe(bind, requireToken(token, logIdentity(mux)))
}

// logIdentity logs the caller identity injected by the auth gateway, when present.
func logIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := r.Header.Get("X-Volume-User"); user != "" {
			// Carry the session id (truncated as in sessionTag) so these lines
			// line up with the MCP method lines from the same client; a GET that
			// opens a notification stream logs nothing else at all.
			sid := r.Header.Get("Mcp-Session-Id")
			if len(sid) > 8 {
				sid = sid[:8]
			}
			if sid == "" {
				// A REST caller has no MCP session, so identify it by address
				// instead — and by the name it claims in X-Client-Id, when it
				// sends one. That name is self-declared: a label for reading
				// logs, never an identity (user= is the authenticated part).
				sid = "REST:" + clientIP(r)
				if tag := clientTag(r.Header.Get("X-Client-Id")); tag != "" {
					sid += ":" + tag
				}
			}
			log.Printf("kbmcp: [%s] %s %s user=%q scopes=%q", sid, r.Method, r.URL.Path,
				user, r.Header.Get("X-Volume-Scopes"))
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
			// Log the rejection so a mis-wired gateway is visible. Distinguish no
			// token (gateway not injecting one) from a wrong token (mismatch
			// between Caddy's and kbmcp's KBMCP_TOKEN, or a client reaching us
			// directly). Never log the token value itself.
			reason := "no bearer token"
			if len(got) > 0 {
				reason = "wrong bearer token"
			}
			log.Printf("kbmcp: 401 %s %s: %s (from %s)", r.Method, r.URL.Path, reason, clientIP(r))
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP is the best-effort caller address for logs: the gateway sets
// X-Forwarded-For with the real client; otherwise it's the direct peer.
// clientTag sanitises the self-declared X-Client-Id before it reaches a log
// line. The value is caller-controlled, so it is capped and stripped of
// anything that could forge a line (newlines, control characters) or blur the
// field separator.
func clientTag(v string) string {
	const maxLen = 32
	var b strings.Builder
	for _, c := range v {
		if c > ' ' && c < 127 && c != ':' && c != '"' {
			b.WriteRune(c)
		}
		if b.Len() >= maxLen {
			break
		}
	}
	return b.String()
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	return r.RemoteAddr
}
