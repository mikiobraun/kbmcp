package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestHistoryAndAuthor(t *testing.T) {
	newRepo(t)
	ctx := context.Background()

	// Two commits by two different agents.
	if _, _, err := WriteFile(ctx, nil, WriteFileInput{
		Path: "a.md", Content: "one\n", Message: "add a", AuthorName: "alice", AuthorEmail: "alice@x",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EditFile(ctx, nil, EditFileInput{
		Path: "a.md", OldString: "one", NewString: "two", Message: "edit a", AuthorName: "bob", AuthorEmail: "bob@x",
	}); err != nil {
		t.Fatal(err)
	}

	_, out, err := History(ctx, nil, HistoryInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Commits) != 2 {
		t.Fatalf("want 2 commits, got %d", len(out.Commits))
	}
	// Newest first: bob's edit, then alice's add.
	if out.Commits[0].Author != "bob" || out.Commits[0].Subject != "edit a" {
		t.Errorf("commit[0] = %+v", out.Commits[0])
	}
	if out.Commits[1].Author != "alice" {
		t.Errorf("commit[1] author = %q, want alice", out.Commits[1].Author)
	}

	// since: only commits after the first one -> just bob's.
	first := out.Commits[1].Hash
	_, since, err := History(ctx, nil, HistoryInput{Since: first})
	if err != nil {
		t.Fatal(err)
	}
	if len(since.Commits) != 1 || since.Commits[0].Author != "bob" {
		t.Fatalf("since = %+v", since.Commits)
	}
}

func TestHistoryPathScopeAndFollow(t *testing.T) {
	dir := newRepo(t)
	ctx := context.Background()
	if _, _, err := WriteFile(ctx, nil, WriteFileInput{Path: "a.md", Content: "a\n", Message: "add a", AuthorEmail: "test@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteFile(ctx, nil, WriteFileInput{Path: "b.md", Content: "b\n", Message: "add b", AuthorEmail: "test@example.com"}); err != nil {
		t.Fatal(err)
	}
	// Rename a.md -> a2.md via git so --follow has something to track.
	if out, err := exec.Command("git", "-C", dir, "mv", "a.md", "a2.md").CombinedOutput(); err != nil {
		t.Fatalf("git mv: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-q", "-m", "rename a").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}

	_, out, err := History(ctx, nil, HistoryInput{Path: "a2.md"})
	if err != nil {
		t.Fatal(err)
	}
	// --follow should surface both the rename and the original creation, not b.md.
	subjects := []string{}
	for _, c := range out.Commits {
		subjects = append(subjects, c.Subject)
	}
	joined := strings.Join(subjects, ",")
	if !strings.Contains(joined, "add a") || !strings.Contains(joined, "rename a") {
		t.Fatalf("follow history = %v", subjects)
	}
	if strings.Contains(joined, "add b") {
		t.Fatalf("path scope leaked b.md: %v", subjects)
	}
}

func TestDiffAndFileAt(t *testing.T) {
	newRepo(t)
	ctx := context.Background()
	if _, _, err := WriteFile(ctx, nil, WriteFileInput{Path: "a.md", Content: "one\n", Message: "add a", AuthorEmail: "test@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EditFile(ctx, nil, EditFileInput{Path: "a.md", OldString: "one", NewString: "two", Message: "edit a", AuthorEmail: "test@example.com"}); err != nil {
		t.Fatal(err)
	}

	// diff HEAD~1..HEAD should show one -> two.
	_, d, err := Diff(ctx, nil, DiffInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Diff, "-one") || !strings.Contains(d.Diff, "+two") {
		t.Fatalf("diff = %q", d.Diff)
	}

	// file_at HEAD~1 should give the old content.
	_, f, err := FileAt(ctx, nil, FileAtInput{Path: "a.md", Ref: "HEAD~1"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Content != "one\n" {
		t.Fatalf("file_at = %q", f.Content)
	}
}

func TestRefValidationRejectsInjection(t *testing.T) {
	newRepo(t)
	ctx := context.Background()
	for _, bad := range []string{"--output=/tmp/x", "-n", "a b", "a;b", "HEAD..HEAD"} {
		if _, _, err := FileAt(ctx, nil, FileAtInput{Path: "a.md", Ref: bad}); err == nil {
			t.Errorf("ref %q was accepted", bad)
		}
		if _, _, err := Diff(ctx, nil, DiffInput{From: bad}); err == nil {
			t.Errorf("diff from %q was accepted", bad)
		}
	}
}
