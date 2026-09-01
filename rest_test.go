package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestGet(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("# Hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setRoot(dir); err != nil {
		t.Fatal(err)
	}

	// happy path: markdown served raw with the right content type
	rec := httptest.NewRecorder()
	restGet(rec, httptest.NewRequest("GET", "/files/note.md", nil))
	if rec.Code != 200 {
		t.Fatalf("note.md: got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("content-type: %q", ct)
	}
	if rec.Body.String() != "# Hi\n" {
		t.Errorf("body: %q", rec.Body.String())
	}
	if rec.Header().Get("ETag") == "" {
		t.Errorf("missing ETag")
	}

	// path confinement: dot-segment escapes must never be served (resolve() guard)
	for _, p := range []string{"/files/../../etc/hostname", "/files/../secret.md"} {
		rec := httptest.NewRecorder()
		restGet(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code == 200 {
			t.Errorf("%s escaped confinement: got 200 %q", p, rec.Body.String())
		}
	}

	// missing file -> 404
	rec = httptest.NewRecorder()
	restGet(rec, httptest.NewRequest("GET", "/files/nope.md", nil))
	if rec.Code != 404 {
		t.Errorf("missing: got %d want 404", rec.Code)
	}
}

func TestRestPut(t *testing.T) {
	newRepo(t) // sets root to a temp git repo with an identity

	// create -> 201, commits, ETag returned
	rec := httptest.NewRecorder()
	restPut(rec, httptest.NewRequest("PUT", "/files/notes/a.md?message=add+a", strings.NewReader("# A\n")))
	if rec.Code != 201 {
		t.Fatalf("create: got %d %q", rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("create: missing ETag")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "notes/a.md")); string(b) != "# A\n" {
		t.Fatalf("content = %q", b)
	}
	if got := gitLog(t, root); len(got) != 1 || got[0] != "add a" {
		t.Fatalf("log = %v", got)
	}

	// missing message -> 400, nothing written
	rec = httptest.NewRecorder()
	restPut(rec, httptest.NewRequest("PUT", "/files/b.md", strings.NewReader("x")))
	if rec.Code != 400 {
		t.Errorf("no message: got %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "b.md")); !os.IsNotExist(err) {
		t.Error("b.md should not exist after 400")
	}

	// If-None-Match: * on an existing file -> 412 (create-only guard)
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/files/notes/a.md?message=nope", strings.NewReader("y"))
	req.Header.Set("If-None-Match", "*")
	restPut(rec, req)
	if rec.Code != 412 {
		t.Errorf("if-none-match: got %d want 412", rec.Code)
	}

	// If-Match with a stale etag -> 412
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("PUT", "/files/notes/a.md?message=stale", strings.NewReader("z"))
	req.Header.Set("If-Match", `"deadbeef-1"`)
	restPut(rec, req)
	if rec.Code != 412 {
		t.Errorf("stale if-match: got %d want 412", rec.Code)
	}

	// If-Match with the current etag -> 200, overwrites and commits
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("PUT", "/files/notes/a.md?message=update+a&author_name=alice&author_email=alice@x", strings.NewReader("# A2\n"))
	req.Header.Set("If-Match", etag)
	restPut(rec, req)
	if rec.Code != 200 {
		t.Fatalf("matched if-match: got %d %q", rec.Code, rec.Body.String())
	}
	if b, _ := os.ReadFile(filepath.Join(root, "notes/a.md")); string(b) != "# A2\n" {
		t.Fatalf("after update = %q", b)
	}
	if got := gitLog(t, root); len(got) != 2 || got[0] != "update a" {
		t.Fatalf("log = %v", got)
	}
}
