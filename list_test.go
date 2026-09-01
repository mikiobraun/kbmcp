package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Dotfiles and dot-directories (e.g. .git) are excluded from listings and
// searches, and a hidden directory is pruned entirely, not just skipped once.
func TestListFilesHidesDotfiles(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "note.md"), []byte("hello secret"), 0o644))
	must(os.WriteFile(filepath.Join(dir, ".env"), []byte("TOKEN=secret"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	must(os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("secret"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "sub", "deep.md"), []byte("x"), 0o644))
	must(setRoot(dir))
	ctx := context.Background()

	_, out, err := ListFiles(ctx, nil, ListInput{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range out.Entries {
		if strings.HasPrefix(e.Path, ".") {
			t.Errorf("hidden entry leaked into listing: %s", e.Path)
		}
	}
	// the real files are still there
	seen := map[string]bool{}
	for _, e := range out.Entries {
		seen[e.Path] = true
	}
	if !seen["note.md"] || !seen[filepath.Join("sub", "deep.md")] {
		t.Errorf("expected real files present, got %v", out.Entries)
	}
}

// sort=modified + sort_reverse=true lists newest first, and paging by next_from
// walks strictly older without gaps or repeats — "give me the latest emails".
func TestListFilesSortModifiedReverse(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// f0 oldest … f9 newest (one day apart).
	for i := 0; i < 10; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%d.md", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := base.AddDate(0, 0, i)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	if err := setRoot(dir); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// First page, newest first.
	_, p1, err := ListFiles(ctx, nil, ListInput{Sort: "modified", Reverse: true, MaxResults: 3})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{p1.Entries[0].Path, p1.Entries[1].Path, p1.Entries[2].Path}
	want := []string{"f9.md", "f8.md", "f7.md"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("page1[%d]: got %s want %s (full %v)", i, got[i], want[i], got)
		}
	}
	if !p1.Truncated {
		t.Fatal("expected truncation")
	}

	// Page through the rest via the cursor; collect the full newest→oldest order.
	order := append([]string{}, p1.Entries[0].Path, p1.Entries[1].Path, p1.Entries[2].Path)
	from := p1.NextFrom
	for {
		_, pg, err := ListFiles(ctx, nil, ListInput{Sort: "modified", Reverse: true, MaxResults: 3, From: from})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range pg.Entries {
			order = append(order, e.Path)
		}
		if !pg.Truncated {
			break
		}
		from = pg.NextFrom
	}
	wantOrder := []string{"f9.md", "f8.md", "f7.md", "f6.md", "f5.md", "f4.md", "f3.md", "f2.md", "f1.md", "f0.md"}
	if fmt.Sprint(order) != fmt.Sprint(wantOrder) {
		t.Errorf("paged newest→oldest order wrong:\n got  %v\n want %v", order, wantOrder)
	}
}

// list_files paginates by path: max_results caps the page, next_from cursors to
// the rest, and the pages together cover every entry exactly once.
func TestListFilesPaging(t *testing.T) {
	dir := t.TempDir()
	// 25 flat files with sortable names.
	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("mail-%02d.md", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := setRoot(dir); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// First page: capped at 10, truncated, cursor points at the 10th path.
	_, page1, err := ListFiles(ctx, nil, ListInput{MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Entries) != 10 || !page1.Truncated {
		t.Fatalf("page1: got %d entries truncated=%v, want 10 truncated=true", len(page1.Entries), page1.Truncated)
	}
	if page1.NextFrom != page1.Entries[9].Path {
		t.Errorf("next_from %q should equal last path %q", page1.NextFrom, page1.Entries[9].Path)
	}

	// Walk all pages via the cursor and collect every path.
	seen := map[string]bool{}
	from := ""
	pages := 0
	for {
		_, pg, err := ListFiles(ctx, nil, ListInput{MaxResults: 10, From: from})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, e := range pg.Entries {
			if seen[e.Path] {
				t.Errorf("duplicate path across pages: %s", e.Path)
			}
			seen[e.Path] = true
			if e.Path <= from {
				t.Errorf("entry %q not strictly after cursor %q", e.Path, from)
			}
		}
		if !pg.Truncated {
			break
		}
		from = pg.NextFrom
	}
	if len(seen) != 25 {
		t.Errorf("paged listing covered %d files, want 25", len(seen))
	}
	if pages != 3 { // 10 + 10 + 5
		t.Errorf("expected 3 pages, got %d", pages)
	}
}

// A default (no max_results) call returns everything untruncated when under the
// default cap.
func TestListFilesDefaultNoTruncate(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.md", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := setRoot(dir); err != nil {
		t.Fatal(err)
	}
	_, out, err := ListFiles(context.Background(), nil, ListInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Truncated || out.NextFrom != "" || len(out.Entries) != 5 {
		t.Errorf("small listing should not truncate: %+v", out)
	}
}
