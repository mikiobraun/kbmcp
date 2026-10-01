package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectStdio connects a client to a server whose scopes are fixed, the way
// stdio runs it.
func connectStdio(t *testing.T, scopes []string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := newServer("", fixedScopes(scopes)).Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// headerTransport sets the headers the gateway would inject on every request.
type headerTransport struct{ header http.Header }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.header {
		r.Header[k] = v
	}
	return http.DefaultTransport.RoundTrip(r)
}

// connectHTTP connects a client through the real HTTP handler, carrying the
// shared token plus whatever scopes header the gateway would pass ("" = none).
func connectHTTP(t *testing.T, scopes *string) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(httpHandler(newServer("", headerScopes), "secret"))
	t.Cleanup(srv.Close)
	h := http.Header{"Authorization": {"Bearer secret"}}
	if scopes != nil {
		h.Set("X-Volume-Scopes", *scopes)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "0"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: &http.Client{Transport: headerTransport{h}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func writeNote(t *testing.T, cs *mcp.ClientSession) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "write_file",
		Arguments: map[string]any{
			"path": "a.md", "content": "# A\n", "message": "add a", "author_email": "probe@example.com",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// A read-only caller is refused with a reason naming the missing scope, and
// the refusal happens before the tool runs: nothing reaches disk.
func TestReadOnlyStdioCannotWrite(t *testing.T) {
	dir := newRepo(t)
	res := writeNote(t, connectStdio(t, []string{scopeRead}))
	if !res.IsError || !strings.Contains(resultText(res), `"write" scope`) {
		t.Fatalf("expected a scope refusal, got error=%v %q", res.IsError, resultText(res))
	}
	if _, err := os.Stat(filepath.Join(dir, "a.md")); !os.IsNotExist(err) {
		t.Errorf("refused write still created the file (stat err %v)", err)
	}
}

// Reading needs no scope at all.
func TestReadNeedsNoScope(t *testing.T) {
	dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := connectStdio(t, nil).CallTool(context.Background(), &mcp.CallToolParams{
		Name: "read_file", Arguments: map[string]any{"paths": []string{"n.md"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("read refused: %q", resultText(res))
	}
}

func TestWriteScopeAllowsStdioWrite(t *testing.T) {
	newRepo(t)
	if res := writeNote(t, connectStdio(t, []string{scopeRead, scopeWrite})); res.IsError {
		t.Fatalf("write refused: %q", resultText(res))
	}
}

// Over HTTP the gateway's header decides. A missing header is read-only — a
// direct caller holding only the shared token gets no write access by default.
func TestHTTPWithoutScopesHeaderIsReadOnly(t *testing.T) {
	newRepo(t)
	if res := writeNote(t, connectHTTP(t, nil)); !res.IsError {
		t.Fatal("write allowed without a scopes header")
	}
}

func TestHTTPReadScopeCannotWrite(t *testing.T) {
	newRepo(t)
	read := "read"
	if res := writeNote(t, connectHTTP(t, &read)); !res.IsError {
		t.Fatal("write allowed with only the read scope")
	}
}

func TestHTTPWriteScopeAllowsWrite(t *testing.T) {
	newRepo(t)
	rw := "read write"
	if res := writeNote(t, connectHTTP(t, &rw)); res.IsError {
		t.Fatalf("write refused: %q", resultText(res))
	}
}

// The REST writes are gated the same way, through the same routes serveHTTP
// mounts.
func TestRESTWriteNeedsWriteScope(t *testing.T) {
	newRepo(t)
	h := httpHandler(newServer("", headerScopes), "secret")
	put := func(scopes string) int {
		req := httptest.NewRequest("PUT", "/files/a.md?message=add+a", strings.NewReader("# A\n"))
		req.Header.Set("Authorization", "Bearer secret")
		if scopes != "" {
			req.Header.Set("X-Volume-Scopes", scopes)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := put(""); code != http.StatusForbidden {
		t.Errorf("no scopes: got %d, want 403", code)
	}
	if code := put("read"); code != http.StatusForbidden {
		t.Errorf("read only: got %d, want 403", code)
	}
	if code := put("read write"); code != http.StatusCreated {
		t.Errorf("read write: got %d, want 201", code)
	}
}

// writeTools is a list kept by hand; a renamed tool would silently drop out of
// it and become writable by anyone. Every name in it must be a real tool.
func TestWriteToolsAreRegistered(t *testing.T) {
	newRepo(t)
	res, err := connectStdio(t, nil).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, tool := range res.Tools {
		registered[tool.Name] = true
	}
	for name := range writeTools {
		if !registered[name] {
			t.Errorf("writeTools names %q, which is not a registered tool", name)
		}
	}
}

func TestScopeFlagRejectsUnknownScope(t *testing.T) {
	if _, err := parseScopeFlag("read wirte"); err == nil {
		t.Error("a misspelled scope was accepted")
	}
	got, err := parseScopeFlag("read write")
	if err != nil || len(got) != 2 {
		t.Errorf("parseScopeFlag(\"read write\") = %q, %v", got, err)
	}
}
