package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
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
