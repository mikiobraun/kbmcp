package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

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
