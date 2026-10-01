package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Write access is the one thing kbmcp gates: reading is always allowed, and
// changing the vault needs the "write" scope. The gateway (volume-auth) only
// authenticates — it forwards the token's scopes in X-Volume-Scopes without
// deciding anything — so what a scope permits is decided here, the one place
// that knows which operations write.
const (
	scopeRead  = "read"
	scopeWrite = "write"
)

// writeTools are the MCP tools that change the vault. Every other tool reads.
// A new tool that writes must be added here, or a read-only caller can use it.
var writeTools = map[string]bool{
	"write_file":  true,
	"edit_file":   true,
	"delete_file": true,
	"move_file":   true,
	"batch_edits": true,
}

// scopesFunc reports the scopes held by the caller of one MCP request. stdio
// and HTTP get theirs from different places — a flag, a header — so the
// server is handed the right one at startup instead of guessing per request.
type scopesFunc func(mcp.Request) []string

// fixedScopes is the stdio source: there is no auth, so whoever starts the
// server decides once, with -scopes, what its single client may do.
func fixedScopes(scopes []string) scopesFunc {
	return func(mcp.Request) []string { return scopes }
}

// headerScopes is the HTTP source: the scopes the gateway injected, trusted
// because the shared token proved the caller is the gateway. A missing or
// empty header means no scopes, which is read-only.
func headerScopes(req mcp.Request) []string {
	extra := req.GetExtra()
	if extra == nil || extra.Header == nil {
		return nil
	}
	return strings.Fields(extra.Header.Get("X-Volume-Scopes"))
}

// parseScopeFlag parses -scopes. Unknown names are an error rather than
// ignored: a typo like "wirte" would otherwise quietly start a read-only
// server whose writes all fail.
func parseScopeFlag(s string) ([]string, error) {
	scopes := strings.Fields(s)
	for _, sc := range scopes {
		if sc != scopeRead && sc != scopeWrite {
			return nil, fmt.Errorf("unknown scope %q in -scopes (known: %q, %q)", sc, scopeRead, scopeWrite)
		}
	}
	return scopes, nil
}

// missingWrite is the refusal both surfaces give, so an agent and a REST
// client read the same reason.
func missingWrite(op string, scopes []string) string {
	return fmt.Sprintf("%s changes the vault and needs the %q scope; this caller has scopes %q, which allow reading only",
		op, scopeWrite, strings.Join(scopes, " "))
}

// scopeMiddleware refuses write tools to a caller without the write scope. The
// refusal is a tool error, not a protocol error, so the agent sees the reason
// as the call's result rather than as a broken connection.
func scopeMiddleware(scopesOf scopesFunc) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && writeTools[p.Name] {
					if scopes := scopesOf(req); !slices.Contains(scopes, scopeWrite) {
						return &mcp.CallToolResult{
							IsError: true,
							Content: []mcp.Content{&mcp.TextContent{Text: missingWrite(p.Name, scopes)}},
						}, nil
					}
				}
			}
			return next(ctx, method, req)
		}
	}
}

// requireWrite wraps a REST handler that changes the vault, answering 403
// unless the gateway passed the write scope.
func requireWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scopes := strings.Fields(r.Header.Get("X-Volume-Scopes"))
		if !slices.Contains(scopes, scopeWrite) {
			msg := missingWrite(r.Method+" "+r.URL.Path, scopes)
			log.Printf("kbmcp: 403 %s", msg)
			http.Error(w, msg, http.StatusForbidden)
			return
		}
		next(w, r)
	}
}
