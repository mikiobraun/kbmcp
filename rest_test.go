package main

import (
	"context"
	"encoding/json"
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

// GET /history returns recent commits as JSON, newest first.
func TestRestHistory(t *testing.T) {
	newRepo(t)
	ctx := context.Background()
	for _, c := range []struct{ path, msg string }{{"a.md", "add a"}, {"b.md", "add b"}} {
		if _, _, err := WriteFile(ctx, nil, WriteFileInput{Path: c.path, Content: "x\n", Message: c.msg, AuthorEmail: "test@example.com"}); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	restHistory(rec, httptest.NewRequest("GET", "/history?max=10", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Commits []Commit `json:"commits"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Commits) != 2 {
		t.Fatalf("want 2 commits, got %d", len(body.Commits))
	}
	if body.Commits[0].Subject != "add b" {
		t.Errorf("newest subject = %q, want 'add b'", body.Commits[0].Subject)
	}
	if body.Commits[0].Hash == "" || body.Commits[0].Relative == "" {
		t.Errorf("commit missing hash/relative: %+v", body.Commits[0])
	}
}

// GET /search returns substring matches with the line they were found on.
func TestRestSearch(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"note.md":       "alpha\nbeta needle here\ngamma\n",
		"sub/other.md":  "nothing\nneedle again\n",
		"unrelated.txt": "no match in here\n",
	}
	for p, content := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := setRoot(dir); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	restSearch(rec, httptest.NewRequest("GET", "/search?substring=needle", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out SearchOutput
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Matches) != 2 {
		t.Fatalf("want 2 matches, got %d: %+v", len(out.Matches), out.Matches)
	}
	// Paths come back relative to root, and the matching line rides along so a
	// caller can show a fragment without re-reading the file.
	byPath := map[string]Match{}
	for _, m := range out.Matches {
		byPath[m.Path] = m
	}
	if m, ok := byPath["note.md"]; !ok || m.Line != 2 || m.Text != "beta needle here" {
		t.Errorf("note.md match wrong: %+v", m)
	}
	if _, ok := byPath["sub/other.md"]; !ok {
		t.Errorf("missing nested match, got %+v", out.Matches)
	}

	// A literal is literal: metacharacters match themselves rather than compiling.
	rec = httptest.NewRecorder()
	restSearch(rec, httptest.NewRequest("GET", "/search?substring=%5Bx%5D", nil)) // [x]
	if rec.Code != 200 {
		t.Fatalf("bracket search status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Matches) != 0 {
		t.Errorf("'[x]' should match nothing literally, got %+v", out.Matches)
	}
	// No matches serialises as [], not null.
	if !strings.Contains(body, `"matches":[]`) {
		t.Errorf("no-match body should carry an empty array: %s", body)
	}

	// max caps the results and says that it did.
	rec = httptest.NewRecorder()
	restSearch(rec, httptest.NewRequest("GET", "/search?substring=needle&max=1", nil))
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Matches) != 1 || !out.Truncated {
		t.Errorf("max=1: got %d matches, truncated=%v", len(out.Matches), out.Truncated)
	}

	// A missing or blank pattern is a bad request, never a search for everything.
	for _, q := range []string{"/search", "/search?substring=", "/search?substring=%20%20"} {
		rec := httptest.NewRecorder()
		restSearch(rec, httptest.NewRequest("GET", q, nil))
		if rec.Code != 400 {
			t.Errorf("%s: got %d, want 400", q, rec.Code)
		}
	}
}

// GET /find matches filenames literally: the pattern is escaped, so a typed
// name is a substring rather than a regex.
func TestRestFind(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"meeting.md", "sub/meeting-notes.md", "sub/other.md", "meetingXmd"} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := setRoot(dir); err != nil {
		t.Fatal(err)
	}

	get := func(q string) FindOutput {
		t.Helper()
		rec := httptest.NewRecorder()
		restFind(rec, httptest.NewRequest("GET", q, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: status %d: %s", q, rec.Code, rec.Body.String())
		}
		var out FindOutput
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := get("/find?substring=meeting")
	if len(out.Paths) != 3 {
		t.Fatalf("want 3 paths, got %v", out.Paths)
	}
	// Nested paths come back relative to root.
	found := strings.Join(out.Paths, ",")
	if !strings.Contains(found, "sub/meeting-notes.md") {
		t.Errorf("missing nested path: %v", out.Paths)
	}

	// '.' is a literal, not "any character": "meetingXmd" must not match.
	out = get("/find?substring=meeting.md")
	if len(out.Paths) != 1 || out.Paths[0] != "meeting.md" {
		t.Errorf("'.' should be literal, got %v", out.Paths)
	}

	// No matches serialises as [], not null.
	rec := httptest.NewRecorder()
	restFind(rec, httptest.NewRequest("GET", "/find?substring=nothinghere", nil))
	if !strings.Contains(rec.Body.String(), `"paths":[]`) {
		t.Errorf("no-match body should carry an empty array: %s", rec.Body.String())
	}

	// max caps the page and says so.
	out = get("/find?substring=meeting&max=1")
	if len(out.Paths) != 1 || !out.Truncated {
		t.Errorf("max=1: got %v truncated=%v", out.Paths, out.Truncated)
	}

	for _, q := range []string{"/find", "/find?substring=", "/find?substring=%20"} {
		rec := httptest.NewRecorder()
		restFind(rec, httptest.NewRequest("GET", q, nil))
		if rec.Code != 400 {
			t.Errorf("%s: got %d, want 400", q, rec.Code)
		}
	}
}

// GET /links resolves a note's wiki-links through the same rule as the tool.
func TestRestLinks(t *testing.T) {
	vaultWith(t, map[string]string{
		"tax/README.md": "tax\n",
		"tax/notes.md":  "see [[README]] and [[/mails/x]] and [[nope]]\n",
		"mails/x.md":    "a mail\n",
	})

	rec := httptest.NewRecorder()
	restLinks(rec, httptest.NewRequest("GET", "/links?path=tax/notes.md", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out OutgoingLinksOutput
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Links) != 3 {
		t.Fatalf("want 3 links, got %+v", out.Links)
	}
	got := map[string]string{}
	for _, l := range out.Links {
		got[l.Target] = l.Resolved
	}
	if got["README"] != "tax/README.md" {
		t.Errorf("sibling name: %+v", out.Links)
	}
	if got["/mails/x"] != "mails/x.md" {
		t.Errorf("vault path: %+v", out.Links)
	}
	if got["nope"] != "" {
		t.Errorf("unresolvable link should be broken: %+v", out.Links)
	}

	// A note with no links answers with [], not null.
	rec = httptest.NewRecorder()
	restLinks(rec, httptest.NewRequest("GET", "/links?path=mails/x.md", nil))
	if !strings.Contains(rec.Body.String(), `"links":[]`) {
		t.Errorf("empty links should serialise as an array: %s", rec.Body.String())
	}

	for _, q := range []string{"/links", "/links?path=", "/links?path=does/not/exist.md"} {
		rec := httptest.NewRecorder()
		restLinks(rec, httptest.NewRequest("GET", q, nil))
		if rec.Code != 400 {
			t.Errorf("%s: got %d, want 400", q, rec.Code)
		}
	}
}

// Gitignored content is still content: a vault may deliberately keep a folder
// out of git (an intake folder that can be re-fetched), and the file listing
// shows it, so search and find must not pretend it isn't there. Hidden files
// stay excluded — that is a different rule, and it still applies.
func TestSearchAndFindSeeGitignoredFiles(t *testing.T) {
	dir := newRepo(t)
	write := func(p, content string) {
		t.Helper()
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "mails/\n")
	write("mails/intake.md", "a needle in the intake folder\n")
	write("kept.md", "a needle in a tracked note\n")
	write(".hidden.md", "a needle nobody should see\n")
	// .ignore is ripgrep's and fd's own ignore file, which kbmcp's Go walkers
	// know nothing about — so it must not decide what search can see either.
	write(".ignore", "drafts/\n")
	write("drafts/wip.md", "a needle in a draft\n")

	rec := httptest.NewRecorder()
	restSearch(rec, httptest.NewRequest("GET", "/search?substring=needle", nil))
	var found SearchOutput
	if err := json.NewDecoder(rec.Body).Decode(&found); err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, m := range found.Matches {
		paths[m.Path] = true
	}
	if !paths["mails/intake.md"] {
		t.Errorf("search skipped a gitignored file: %v", found.Matches)
	}
	if !paths["kept.md"] {
		t.Errorf("search missed a tracked file: %v", found.Matches)
	}
	if !paths["drafts/wip.md"] {
		t.Errorf("search obeyed an .ignore file: %v", found.Matches)
	}
	if paths[".hidden.md"] {
		t.Errorf("search should still skip hidden files: %v", found.Matches)
	}

	rec = httptest.NewRecorder()
	restFind(rec, httptest.NewRequest("GET", "/find?substring=.md", nil))
	var names FindOutput
	if err := json.NewDecoder(rec.Body).Decode(&names); err != nil {
		t.Fatal(err)
	}
	list := strings.Join(names.Paths, ",")
	if !strings.Contains(list, "mails/intake.md") {
		t.Errorf("find skipped a gitignored file: %v", names.Paths)
	}
	if !strings.Contains(list, "drafts/wip.md") {
		t.Errorf("find obeyed an .ignore file: %v", names.Paths)
	}
	if strings.Contains(list, ".hidden.md") {
		t.Errorf("find should still skip hidden files: %v", names.Paths)
	}
}
