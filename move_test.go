package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func move(t *testing.T, from, to string) MoveFileOutput {
	t.Helper()
	_, out, err := MoveFile(context.Background(), nil, MoveFileInput{
		From: from, To: to, Message: "move " + from, AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMoveFileRenamesAndRewritesEveryFormOfIncomingLink(t *testing.T) {
	dir := newRepo(t)
	seed(t, "concepts/old.md", "the note\n")
	seed(t, "a.md", "[[old]] [[Old|Alias]] [[old#Sec]] [[concepts/old]] [[/concepts/old.md]]\n"+
		"code `[[old]]` stays\n")
	seed(t, "concepts/b.md", "[[./old]] and [[ old ^block]]\n")

	out := move(t, "concepts/old.md", "concepts/new.md")

	if got, want := read(t, "a.md"), "[[new]] [[new|Alias]] [[new#Sec]] [[concepts/new]] [[/concepts/new.md]]\n"+
		"code `[[old]]` stays\n"; got != want {
		t.Errorf("a.md =\n%s\nwant\n%s", got, want)
	}
	if got, want := read(t, "concepts/b.md"), "[[./new]] and [[ new ^block]]\n"; got != want {
		t.Errorf("b.md = %q, want %q", got, want)
	}
	if exists(filepath.Join(dir, "concepts/old.md")) || read(t, "concepts/new.md") != "the note\n" {
		t.Error("the file should have moved with its content")
	}
	if len(out.Links) != 7 || !out.Committed {
		t.Errorf("links=%+v committed=%v", out.Links, out.Committed)
	}
	if got := gitLog(t, dir); len(got) != 4 || got[0] != "move concepts/old.md" {
		t.Errorf("log = %v, want the move and the rewrites as one commit", got)
	}
	// git pairs the removal and the addition up as a rename, so history follows it.
	status, err := exec.Command("git", "-C", dir, "show", "--name-status", "--format=", "-M", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(status), "R100\tconcepts/old.md\tconcepts/new.md") {
		t.Errorf("HEAD is not a rename:\n%s", status)
	}
}

// A new name in a folder shadows the vault-wide note a sibling's link used to
// find, and a duplicated name makes a unique one ambiguous. Neither link ever
// pointed at the moved note, and both must keep their target.
func TestMoveFileKeepsLinksTheMoveWouldCapture(t *testing.T) {
	newRepo(t)
	seed(t, "ideas/foo.md", "the real foo\n")
	seed(t, "x/bar.md", "bar\n")
	seed(t, "tax/notes.md", "see [[foo]]\n")
	seed(t, "top.md", "see [[foo|the foo]]\n")

	out := move(t, "x/bar.md", "tax/foo.md")

	if got := read(t, "tax/notes.md"); got != "see [[ideas/foo]]\n" {
		t.Errorf("sibling shadowing: tax/notes.md = %q", got)
	}
	if got := read(t, "top.md"); got != "see [[ideas/foo|the foo]]\n" {
		t.Errorf("ambiguity: top.md = %q", got)
	}
	if len(out.Links) != 2 {
		t.Errorf("links = %+v", out.Links)
	}
}

// The moved note's own links resolve from its new folder: ./ and sibling names
// have to be respelled, a vault-wide name does not.
func TestMoveFileRewritesTheMovedNotesOwnRelativeLinks(t *testing.T) {
	newRepo(t)
	seed(t, "a/README.md", "a\n")
	seed(t, "b/README.md", "b\n")
	seed(t, "a/sib.md", "sib\n")
	seed(t, "c/unique.md", "u\n")
	seed(t, "a/note.md", "[[./sib]] [[README]] [[unique]] [[note]]\n")

	move(t, "a/note.md", "b/note.md")

	if got, want := read(t, "b/note.md"), "[[sib]] [[a/README]] [[unique]] [[note]]\n"; got != want {
		t.Errorf("b/note.md = %q, want %q", got, want)
	}
}

// A self-link by name follows the rename.
func TestMoveFileRewritesSelfLinks(t *testing.T) {
	newRepo(t)
	seed(t, "old.md", "I am [[old]]\n")
	move(t, "old.md", "new.md")
	if got := read(t, "new.md"); got != "I am [[new]]\n" {
		t.Errorf("new.md = %q", got)
	}
}

// A broken link has no target to keep — including one broken by ambiguity,
// which the move happens to settle.
func TestMoveFileLeavesBrokenLinksAlone(t *testing.T) {
	newRepo(t)
	seed(t, "a/dup.md", "a\n")
	seed(t, "b/dup.md", "b\n")
	seed(t, "top.md", "[[dup]] [[missing]]\n")

	out := move(t, "a/dup.md", "a/other.md")

	if got := read(t, "top.md"); got != "[[dup]] [[missing]]\n" {
		t.Errorf("top.md = %q", got)
	}
	if len(out.Links) != 0 {
		t.Errorf("links = %+v", out.Links)
	}
}

// With no sibling and an ambiguous name, a moved-to-root note is reached by a
// vault path — which at the root has no slash, so it needs the leading one.
func TestMoveFileFallsBackToARootedPath(t *testing.T) {
	newRepo(t)
	seed(t, "a/x.md", "x\n")
	seed(t, "b/x.md", "other x\n")
	seed(t, "c/linker.md", "[[a/x]]\n")

	move(t, "a/x.md", "x.md")

	if got := read(t, "c/linker.md"); got != "[[/x]]\n" {
		t.Errorf("linker = %q", got)
	}
}

func TestMoveFileCreatesAndPrunesFolders(t *testing.T) {
	dir := newRepo(t)
	seed(t, "old/deep/n.md", "n\n")
	move(t, "old/deep/n.md", "new/place/n.md")
	if exists(filepath.Join(dir, "old")) {
		t.Error("old/ should be pruned once empty")
	}
	if read(t, "new/place/n.md") != "n\n" {
		t.Error("destination folders should be created")
	}
}

func TestMoveFileDryRun(t *testing.T) {
	dir := newRepo(t)
	seed(t, "old.md", "o\n")
	seed(t, "a.md", "[[old]]\n")

	_, out, err := MoveFile(context.Background(), nil, MoveFileInput{From: "old.md", To: "new.md", Message: "m", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Links) != 1 || out.Links[0].Path != "a.md" || out.Links[0].Old != "[[old]]" || out.Links[0].New != "[[new]]" {
		t.Errorf("links = %+v", out.Links)
	}
	if out.Committed || !exists(filepath.Join(dir, "old.md")) || read(t, "a.md") != "[[old]]\n" || len(gitLog(t, dir)) != 2 {
		t.Error("a dry run must not write or commit")
	}
}

func TestMoveFileRefusals(t *testing.T) {
	dir := newRepo(t)
	ctx := context.Background()
	seed(t, "a.md", "a\n")
	seed(t, "taken.md", "t\n")
	seed(t, "folder/x.md", "x\n")
	seed(t, "linker.md", "[[a]]\n")

	try := func(from, to string) error {
		t.Helper()
		_, _, err := MoveFile(ctx, nil, MoveFileInput{From: from, To: to, Message: "m", AuthorEmail: "test@example.com"})
		return err
	}

	if err := try("a.md", "taken.md"); !errors.Is(err, errTargetExists) {
		t.Errorf("taken: err = %v", err)
	}
	if err := try("a.md", "folder"); !errors.Is(err, errTargetExists) {
		t.Errorf("onto a folder: err = %v", err)
	}
	if err := try("missing.md", "b.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: err = %v", err)
	}
	for _, c := range [][2]string{
		{"folder", "folder2"}, {"a.md", "a.md"}, {"a.md", ".hidden.md"},
		{"a.md", "../out.md"},
	} {
		if err := try(c[0], c[1]); err == nil {
			t.Errorf("move %s to %s should fail", c[0], c[1])
		}
	}

	if err := os.Symlink("a.md", filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := try("link.md", "link2.md"); err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Errorf("moving a symlink: err = %v", err)
	}
	if err := try("a.md", "b.md"); err == nil || !strings.Contains(err.Error(), "target of the symlink link.md") {
		t.Errorf("moving a symlink's target: err = %v", err)
	}
	if err := try("a.md", "taken.md/inside.md"); err == nil || !strings.Contains(err.Error(), "taken.md is a file") {
		t.Errorf("into a file: err = %v", err)
	}
	os.Remove(filepath.Join(dir, "link.md"))

	// Uncommitted content, in the moved file or a note whose link must change,
	// would be swept into the move's commit.
	if err := os.WriteFile(filepath.Join(dir, "linker.md"), []byte("[[a]] edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := try("a.md", "b.md"); !errors.Is(err, errUncommitted) {
		t.Errorf("uncommitted linker: err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "taken.md"), []byte("t2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := try("taken.md", "t.md"); !errors.Is(err, errUncommitted) {
		t.Errorf("uncommitted source: err = %v", err)
	}

	if len(gitLog(t, dir)) != 4 || !exists(filepath.Join(dir, "a.md")) {
		t.Error("refused moves must write and commit nothing")
	}
}

// Inside a batch a move sees what earlier ops left: a note written by the batch
// has its link rewritten, and a chain of moves is reported as one rename.
func TestBatchEditsMoveFollowsEvolvingState(t *testing.T) {
	dir := newRepo(t)
	seed(t, "a.md", "a\n")

	_, out, err := BatchEdits(context.Background(), nil, BatchEditsInput{
		AuthorEmail: "test@example.com", Message: "shuffle",
		Ops: []BatchOp{
			{Op: "write", Path: "index.md", Content: "see [[a]]\n"},
			{Op: "edit", Path: "a.md", OldString: "a", NewString: "A"},
			{Op: "move", Path: "a.md", To: "b.md"},
			{Op: "move", Path: "b.md", To: "sub/c.md"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, "index.md"); got != "see [[c]]\n" {
		t.Errorf("index.md = %q", got)
	}
	if got := read(t, "sub/c.md"); got != "A\n" {
		t.Errorf("sub/c.md = %q", got)
	}
	var renamed *BatchFileResult
	for i, f := range out.Files {
		if f.Path == "a.md" || f.Path == "b.md" {
			t.Errorf("intermediate or source path reported: %+v", f)
		}
		if f.Path == "sub/c.md" {
			renamed = &out.Files[i]
		}
	}
	if renamed == nil || renamed.RenamedFrom != "a.md" || renamed.Created || !strings.Contains(renamed.Diff, "+ A") {
		t.Errorf("files = %+v", out.Files)
	}
	if len(out.Links) != 2 {
		t.Errorf("links = %+v", out.Links)
	}
	if got := gitTracked(t, dir); strings.Join(got, ",") != "index.md,sub/c.md" {
		t.Errorf("tracked = %v", got)
	}
}

func TestBatchEditsMoveRefusesATakenDestinationAndWritesNothing(t *testing.T) {
	dir := newRepo(t)
	seed(t, "a.md", "a\n")
	_, _, err := BatchEdits(context.Background(), nil, BatchEditsInput{
		AuthorEmail: "test@example.com", Message: "m",
		Ops: []BatchOp{
			{Op: "write", Path: "b.md", Content: "b\n"},
			{Op: "move", Path: "a.md", To: "b.md"},
		},
	})
	if !errors.Is(err, errTargetExists) {
		t.Fatalf("err = %v", err)
	}
	if exists(filepath.Join(dir, "b.md")) || len(gitLog(t, dir)) != 1 {
		t.Error("a refused batch must write and commit nothing")
	}
}

// A link's offsets point at its text in the note even after a code span on the
// same line, which parsing blanks out.
func TestParseLinksOffsets(t *testing.T) {
	src := "first\n`code` then [[target|x]] and [[b]]\n"
	links := parseLinks([]byte(src))
	if len(links) != 2 {
		t.Fatalf("links = %+v", links)
	}
	for _, l := range links {
		if src[l.Start:l.End] != l.Raw {
			t.Errorf("offsets %d:%d give %q, want %q", l.Start, l.End, src[l.Start:l.End], l.Raw)
		}
	}
}

func TestRestMove(t *testing.T) {
	newRepo(t)
	seed(t, "notes/a.md", "# A\n")
	seed(t, "b.md", "[[a]]\n")
	seed(t, "taken.md", "t\n")

	post := func(target, ifMatch string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", target, nil)
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		restMove(rec, req)
		return rec
	}

	for target, want := range map[string]int{
		"/move?from=notes/a.md&to=notes/c.md":         400,
		"/move?from=nope.md&to=x.md&message=m":        404,
		"/move?from=notes/a.md&to=taken.md&message=m": 409,
		"/move?from=notes&to=folder&message=m":        400,
	} {
		if rec := post(target, ""); rec.Code != want {
			t.Errorf("%s: got %d %q, want %d", target, rec.Code, rec.Body.String(), want)
		}
	}
	if rec := post("/move?from=notes/a.md&to=notes/c.md&message=m", `"deadbeef-1"`); rec.Code != 412 {
		t.Errorf("stale if-match: got %d", rec.Code)
	}

	rec := post("/move?from=notes/a.md&to=notes/c.md&message=rename&dry_run=true", "")
	var dry MoveFileOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &dry); err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || dry.Committed || len(dry.Links) != 1 || dry.Links[0].New != "[[c]]" {
		t.Errorf("dry run = %+v", dry)
	}
	if len(gitLog(t, root)) != 3 {
		t.Error("dry run must not commit")
	}

	get := httptest.NewRecorder()
	restGet(get, httptest.NewRequest("GET", "/files/notes/a.md", nil))
	rec = post("/move?from=notes/a.md&to=notes/c.md&message=rename&author_email=alice@x", get.Header().Get("ETag"))
	if rec.Code != 200 {
		t.Fatalf("move: got %d %q", rec.Code, rec.Body.String())
	}
	var out MoveFileOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Committed || out.From != "notes/a.md" || out.To != "notes/c.md" {
		t.Errorf("out = %+v", out)
	}
	if got := gitLog(t, root); len(got) != 4 || got[0] != "rename" {
		t.Errorf("log = %v", got)
	}
}
