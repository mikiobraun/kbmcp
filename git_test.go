package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo makes a temp git repo with a committer identity and sets it as root.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "test"},
		{"config", "user.email", "test@example.com"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := setRoot(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func gitLog(t *testing.T, dir string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "log", "--pretty=%s").CombinedOutput()
	if err != nil {
		return nil // no commits yet
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestWriteFileCommits(t *testing.T) {
	dir := newRepo(t)
	_, out, err := WriteFile(context.Background(), nil, WriteFileInput{
		Path: "notes/a.md", Content: "# A\n", Message: "add a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Created || !out.Committed {
		t.Fatalf("created=%v committed=%v", out.Created, out.Committed)
	}
	if got := gitLog(t, dir); len(got) != 1 || got[0] != "add a" {
		t.Fatalf("log = %v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "notes/a.md")); string(b) != "# A\n" {
		t.Fatalf("content = %q", b)
	}
}

func TestWriteFileMessageRequired(t *testing.T) {
	newRepo(t)
	if _, _, err := WriteFile(context.Background(), nil, WriteFileInput{Path: "a.md", Content: "x"}); err == nil {
		t.Fatal("expected error for missing message")
	}
}

func TestWriteFileDryRunNoCommit(t *testing.T) {
	dir := newRepo(t)
	_, out, err := WriteFile(context.Background(), nil, WriteFileInput{
		Path: "a.md", Content: "x", Message: "m", DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Committed {
		t.Fatal("dry run should not commit")
	}
	if _, err := os.Stat(filepath.Join(dir, "a.md")); !os.IsNotExist(err) {
		t.Fatal("dry run should not write the file")
	}
	if len(gitLog(t, dir)) != 0 {
		t.Fatal("dry run should leave no commits")
	}
}

func TestEditFileCommits(t *testing.T) {
	dir := newRepo(t)
	if _, _, err := WriteFile(context.Background(), nil, WriteFileInput{Path: "a.md", Content: "hello world\n", Message: "init"}); err != nil {
		t.Fatal(err)
	}
	_, out, err := EditFile(context.Background(), nil, EditFileInput{
		Path: "a.md", OldString: "world", NewString: "there", Message: "edit a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Committed || out.Replacements != 1 {
		t.Fatalf("committed=%v repl=%d", out.Committed, out.Replacements)
	}
	if got := gitLog(t, dir); len(got) != 2 || got[0] != "edit a" {
		t.Fatalf("log = %v", got)
	}
}

func TestBatchEditsAtomicCommit(t *testing.T) {
	dir := newRepo(t)
	// Seed a file to edit.
	if _, _, err := WriteFile(context.Background(), nil, WriteFileInput{Path: "a.md", Content: "one two\n", Message: "seed"}); err != nil {
		t.Fatal(err)
	}
	_, out, err := BatchEdits(context.Background(), nil, BatchEditsInput{
		Message: "batch",
		Ops: []BatchOp{
			{Op: "write", Path: "b.md", Content: "B\n"},
			{Op: "edit", Path: "a.md", OldString: "two", NewString: "TWO"},
			{Op: "edit", Path: "b.md", OldString: "B", NewString: "BEE"}, // stacked edit on just-written file
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Committed || len(out.Files) != 2 {
		t.Fatalf("committed=%v files=%d", out.Committed, len(out.Files))
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "b.md")); string(b) != "BEE\n" {
		t.Fatalf("b.md = %q (stacked edit failed)", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.md")); string(b) != "one TWO\n" {
		t.Fatalf("a.md = %q", b)
	}
	// One commit for the whole batch (seed + batch = 2 total).
	if got := gitLog(t, dir); len(got) != 2 || got[0] != "batch" {
		t.Fatalf("log = %v", got)
	}
}

func TestBatchEditsAbortsOnBadOp(t *testing.T) {
	dir := newRepo(t)
	_, _, err := BatchEdits(context.Background(), nil, BatchEditsInput{
		Message: "batch",
		Ops: []BatchOp{
			{Op: "write", Path: "good.md", Content: "ok\n"},
			{Op: "edit", Path: "missing.md", OldString: "nope", NewString: "x"},
		},
	})
	if err == nil {
		t.Fatal("expected error from bad op")
	}
	// Atomic: the earlier good write must not have hit disk or been committed.
	if _, statErr := os.Stat(filepath.Join(dir, "good.md")); !os.IsNotExist(statErr) {
		t.Fatal("good.md should not exist after aborted batch")
	}
	if len(gitLog(t, dir)) != 0 {
		t.Fatal("aborted batch should leave no commits")
	}
}
