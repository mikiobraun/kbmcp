package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// seed writes and commits a file through the real write path.
func seed(t *testing.T, path, content string) {
	t.Helper()
	if _, _, err := WriteFile(context.Background(), nil, WriteFileInput{
		Path: path, Content: content, Message: "seed " + path, AuthorEmail: "test@example.com",
	}); err != nil {
		t.Fatal(err)
	}
}

func gitTracked(t *testing.T, dir string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "ls-files").CombinedOutput()
	if err != nil {
		t.Fatalf("git ls-files: %v: %s", err, out)
	}
	return strings.Fields(string(out))
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestDeleteFileCommitsAndPrunesEmptyFolders(t *testing.T) {
	dir := newRepo(t)
	seed(t, "a/b/c.md", "gone soon\n")
	seed(t, "keep.md", "x\n")

	_, out, err := DeleteFile(context.Background(), nil, DeleteFileInput{
		Path: "a/b/c.md", Message: "delete c", AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Committed || !strings.Contains(out.Diff, "- gone soon") {
		t.Fatalf("committed=%v diff=%q", out.Committed, out.Diff)
	}
	if exists(filepath.Join(dir, "a")) {
		t.Error("a/ should be pruned once empty")
	}
	if got := gitLog(t, dir); got[0] != "delete c" {
		t.Errorf("log = %v", got)
	}
	if got := gitTracked(t, dir); len(got) != 1 || got[0] != "keep.md" {
		t.Errorf("tracked = %v", got)
	}
}

func TestDeleteFileKeepsNonEmptyFolders(t *testing.T) {
	dir := newRepo(t)
	seed(t, "a/x.md", "x\n")
	seed(t, "a/y.md", "y\n")
	if _, _, err := DeleteFile(context.Background(), nil, DeleteFileInput{
		Path: "a/x.md", Message: "delete x", AuthorEmail: "test@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "a/y.md")) {
		t.Error("a/y.md should survive")
	}
}

// Deleting is only safe because history can bring the file back, so anything
// history does not hold is refused.
func TestDeleteFileRefusesUncommittedContent(t *testing.T) {
	dir := newRepo(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(dir, "untracked.md"), []byte("u\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := DeleteFile(ctx, nil, DeleteFileInput{Path: "untracked.md", Message: "m", AuthorEmail: "test@example.com"})
	if !errors.Is(err, errUncommitted) {
		t.Errorf("untracked: err = %v, want errUncommitted", err)
	}

	seed(t, "modified.md", "v1\n")
	if err := os.WriteFile(filepath.Join(dir, "modified.md"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = DeleteFile(ctx, nil, DeleteFileInput{Path: "modified.md", Message: "m", AuthorEmail: "test@example.com"})
	if !errors.Is(err, errUncommitted) {
		t.Errorf("modified: err = %v, want errUncommitted", err)
	}

	// A dry run applies the same check, so it cannot promise a deletion that
	// would then be refused.
	_, _, err = DeleteFile(ctx, nil, DeleteFileInput{Path: "modified.md", Message: "m", DryRun: true})
	if !errors.Is(err, errUncommitted) {
		t.Errorf("dry run: err = %v, want errUncommitted", err)
	}

	if !exists(filepath.Join(dir, "untracked.md")) || !exists(filepath.Join(dir, "modified.md")) {
		t.Error("refused deletions must leave the files in place")
	}
}

func TestDeleteFileRefusesDirectoriesHiddenAndMissing(t *testing.T) {
	newRepo(t)
	seed(t, "notes/a.md", "a\n")
	ctx := context.Background()
	for _, p := range []string{"notes", "notes/", ".git/config", "../x.md", "", "missing.md"} {
		if _, _, err := DeleteFile(ctx, nil, DeleteFileInput{Path: p, Message: "m", AuthorEmail: "test@example.com"}); err == nil {
			t.Errorf("delete %q should fail", p)
		}
	}
}

func TestDeleteFileDryRun(t *testing.T) {
	dir := newRepo(t)
	seed(t, "a.md", "a\n")
	_, out, err := DeleteFile(context.Background(), nil, DeleteFileInput{Path: "a.md", Message: "m", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Committed || !strings.Contains(out.Diff, "- a") {
		t.Fatalf("committed=%v diff=%q", out.Committed, out.Diff)
	}
	if !exists(filepath.Join(dir, "a.md")) || len(gitLog(t, dir)) != 1 {
		t.Error("dry run must not delete or commit")
	}
}

// A symlink is its own entry, to git as to rm: deleting it removes the link and
// leaves the file it points to alone.
func TestDeleteFileRemovesSymlinkNotTarget(t *testing.T) {
	dir := newRepo(t)
	seed(t, "target.md", "t\n")
	if err := os.Symlink("target.md", filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "link.md"}, {"commit", "-q", "-m", "add link"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	_, out, err := DeleteFile(context.Background(), nil, DeleteFileInput{
		Path: "link.md", Message: "delete link", AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Path != "link.md" || !out.Committed {
		t.Fatalf("path=%q committed=%v", out.Path, out.Committed)
	}
	if exists(filepath.Join(dir, "link.md")) {
		t.Error("link should be gone")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "target.md")); string(b) != "t\n" {
		t.Errorf("target = %q, should be untouched", b)
	}
	if got := gitTracked(t, dir); len(got) != 1 || got[0] != "target.md" {
		t.Errorf("tracked = %v", got)
	}
}

func TestBatchEditsDeleteInOneCommit(t *testing.T) {
	dir := newRepo(t)
	seed(t, "old/a.md", "a\n")
	seed(t, "b.md", "one\n")
	_, out, err := BatchEdits(context.Background(), nil, BatchEditsInput{
		AuthorEmail: "test@example.com", Message: "move a into b",
		Ops: []BatchOp{
			{Op: "edit", Path: "b.md", OldString: "one", NewString: "one\na"},
			{Op: "delete", Path: "old/a.md"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Committed || len(out.Files) != 2 || !out.Files[1].Deleted || out.Files[1].Created {
		t.Fatalf("out = %+v", out)
	}
	if exists(filepath.Join(dir, "old")) {
		t.Error("old/ should be pruned")
	}
	if got := gitLog(t, dir); len(got) != 3 || got[0] != "move a into b" {
		t.Errorf("log = %v", got)
	}
	if got := gitTracked(t, dir); len(got) != 1 || got[0] != "b.md" {
		t.Errorf("tracked = %v", got)
	}
}

// Ops see the state earlier ops left behind, a deletion included.
func TestBatchEditsDeleteFollowsEvolvingState(t *testing.T) {
	dir := newRepo(t)
	seed(t, "a.md", "a\n")
	ctx := context.Background()

	mustFail := func(name string, ops ...BatchOp) {
		t.Helper()
		if _, _, err := BatchEdits(ctx, nil, BatchEditsInput{AuthorEmail: "test@example.com", Message: "m", Ops: ops}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	mustFail("edit after delete", BatchOp{Op: "delete", Path: "a.md"}, BatchOp{Op: "edit", Path: "a.md", OldString: "a", NewString: "b"})
	mustFail("delete twice", BatchOp{Op: "delete", Path: "a.md"}, BatchOp{Op: "delete", Path: "a.md"})
	mustFail("delete never-there", BatchOp{Op: "delete", Path: "nope.md"})
	mustFail("delete a folder", BatchOp{Op: "write", Path: "d/x.md", Content: "x"}, BatchOp{Op: "delete", Path: "d"})

	// Delete then write recreates the file: a net update, not a deletion.
	_, out, err := BatchEdits(ctx, nil, BatchEditsInput{
		AuthorEmail: "test@example.com", Message: "replace",
		Ops: []BatchOp{{Op: "delete", Path: "a.md"}, {Op: "write", Path: "a.md", Content: "fresh\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Files) != 1 || out.Files[0].Deleted || out.Files[0].Created {
		t.Fatalf("files = %+v", out.Files)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.md")); string(b) != "fresh\n" {
		t.Errorf("a.md = %q", b)
	}

	// A file written and deleted within the batch never reaches disk or history.
	before := len(gitLog(t, dir))
	_, out, err = BatchEdits(ctx, nil, BatchEditsInput{
		AuthorEmail: "test@example.com", Message: "noop",
		Ops: []BatchOp{{Op: "write", Path: "tmp/t.md", Content: "t"}, {Op: "delete", Path: "tmp/t.md"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Committed || len(out.Files) != 0 || exists(filepath.Join(dir, "tmp")) || len(gitLog(t, dir)) != before {
		t.Errorf("committed=%v files=%+v", out.Committed, out.Files)
	}
}

func TestBatchEditsDeleteRefusesUncommittedAndWritesNothing(t *testing.T) {
	dir := newRepo(t)
	seed(t, "a.md", "v1\n")
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The edit makes the in-memory content differ from disk too; what the
	// deletion would lose is the uncommitted v2 on disk, so it is still refused.
	_, _, err := BatchEdits(context.Background(), nil, BatchEditsInput{
		AuthorEmail: "test@example.com", Message: "m",
		Ops: []BatchOp{
			{Op: "write", Path: "good.md", Content: "ok\n"},
			{Op: "edit", Path: "a.md", OldString: "v2", NewString: "v3"},
			{Op: "delete", Path: "a.md"},
		},
	})
	if !errors.Is(err, errUncommitted) {
		t.Fatalf("err = %v, want errUncommitted", err)
	}
	if exists(filepath.Join(dir, "good.md")) || len(gitLog(t, dir)) != 1 {
		t.Error("a refused batch must write and commit nothing")
	}
}

func TestRestDelete(t *testing.T) {
	newRepo(t)
	seed(t, "notes/a.md", "# A\n")
	if err := os.WriteFile(filepath.Join(root, "untracked.md"), []byte("u\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	del := func(target, ifMatch string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("DELETE", target, nil)
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		restDelete(rec, req)
		return rec
	}

	if rec := del("/files/notes/a.md", ""); rec.Code != 400 {
		t.Errorf("no message: got %d", rec.Code)
	}
	if rec := del("/files/nope.md?message=m", ""); rec.Code != 404 {
		t.Errorf("missing: got %d", rec.Code)
	}
	if rec := del("/files/notes?message=m", ""); rec.Code != 400 {
		t.Errorf("directory: got %d", rec.Code)
	}
	if rec := del("/files/untracked.md?message=m", ""); rec.Code != 409 {
		t.Errorf("untracked: got %d", rec.Code)
	}
	if rec := del("/files/notes/a.md?message=m", `"deadbeef-1"`); rec.Code != 412 {
		t.Errorf("stale if-match: got %d", rec.Code)
	}

	get := httptest.NewRecorder()
	restGet(get, httptest.NewRequest("GET", "/files/notes/a.md", nil))
	rec := del("/files/notes/a.md?message=delete+a&author_email=alice@x", get.Header().Get("ETag"))
	if rec.Code != 200 {
		t.Fatalf("delete: got %d %q", rec.Code, rec.Body.String())
	}
	if exists(filepath.Join(root, "notes")) {
		t.Error("notes/ should be pruned")
	}
	if got := gitLog(t, root); len(got) != 2 || got[0] != "delete a" {
		t.Errorf("log = %v", got)
	}
}
