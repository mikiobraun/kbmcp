package main

import (
	"fmt"
	"strings"
	"testing"
)

func requireFd(t *testing.T) {
	t.Helper()
	if _, err := fdBinary(); err != nil {
		t.Skip("fd (or fdfind) not installed")
	}
}

// glob matches by filename across the tree; results are relative to root, sorted.
func TestFindGlob(t *testing.T) {
	requireFd(t)
	ctx := searchVault(t, map[string]string{
		"a.md": "", "b.txt": "", "sub/c.md": "",
	})
	_, out, err := FindFiles(ctx, nil, FindInput{Glob: "*.md"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(out.Paths) != fmt.Sprint([]string{"a.md", "sub/c.md"}) {
		t.Errorf("glob *.md: got %v", out.Paths)
	}
}

// type=dir finds directories, not files.
func TestFindTypeDir(t *testing.T) {
	requireFd(t)
	ctx := searchVault(t, map[string]string{"a.md": "", "sub/c.md": ""})
	_, out, err := FindFiles(ctx, nil, FindInput{Type: "dir"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(out.Paths) != fmt.Sprint([]string{"sub"}) {
		t.Errorf("type=dir: got %v", out.Paths)
	}
}

// path scopes discovery to a subtree.
func TestFindScope(t *testing.T) {
	requireFd(t)
	ctx := searchVault(t, map[string]string{"top.md": "", "sub/c.md": "", "sub/deep/d.md": ""})
	_, out, err := FindFiles(ctx, nil, FindInput{Path: "sub"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range out.Paths {
		if !strings.HasPrefix(p, "sub/") {
			t.Errorf("scope leaked: %v", out.Paths)
		}
	}
	if len(out.Paths) != 2 {
		t.Errorf("expected 2 under sub/, got %v", out.Paths)
	}
}

// hidden files and .git are skipped.
func TestFindHidesDotfiles(t *testing.T) {
	requireFd(t)
	ctx := searchVault(t, map[string]string{
		"a.md": "", ".hidden.md": "", ".git/config": "",
	})
	_, out, err := FindFiles(ctx, nil, FindInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range out.Paths {
		if strings.HasPrefix(p, ".") {
			t.Errorf("hidden path returned: %s", p)
		}
	}
	if fmt.Sprint(out.Paths) != fmt.Sprint([]string{"a.md"}) {
		t.Errorf("expected only a.md, got %v", out.Paths)
	}
}

// max_results caps the page and next_from walks the rest without gaps.
func TestFindPagination(t *testing.T) {
	requireFd(t)
	files := map[string]string{}
	for i := 0; i < 10; i++ {
		files[fmt.Sprintf("f%02d.md", i)] = ""
	}
	ctx := searchVault(t, files)

	seen := map[string]bool{}
	from := ""
	pages := 0
	for {
		_, out, err := FindFiles(ctx, nil, FindInput{MaxResults: 4, From: from})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, p := range out.Paths {
			if seen[p] {
				t.Errorf("duplicate path across pages: %s", p)
			}
			seen[p] = true
		}
		if !out.Truncated {
			break
		}
		from = out.NextFrom
	}
	if len(seen) != 10 || pages != 3 {
		t.Errorf("expected 10 files over 3 pages, got %d over %d", len(seen), pages)
	}
}

// path can't escape the vault (resolve guard), and a bad regex is a real error.
func TestFindConfinementAndBadRegex(t *testing.T) {
	requireFd(t)
	ctx := searchVault(t, map[string]string{"a.md": ""})
	for _, bad := range []string{"..", "../..", "/etc"} {
		if _, _, err := FindFiles(ctx, nil, FindInput{Path: bad}); err == nil {
			t.Errorf("path %q should be rejected", bad)
		}
	}
	if _, _, err := FindFiles(ctx, nil, FindInput{Regex: "("}); err == nil {
		t.Error("expected an error for an unterminated group")
	}
}

// regex and glob are alternatives, not a pattern plus a modifier: naming both
// is an error, naming neither still lists the scope.
func TestFindRejectsBothModes(t *testing.T) {
	requireFd(t)
	ctx := searchVault(t, map[string]string{"a.md": "", "b.txt": ""})
	_, _, err := FindFiles(ctx, nil, FindInput{Regex: `\.md$`, Glob: "*.md"})
	if err == nil {
		t.Fatal("expected an error when both regex and glob are given")
	}
	if !strings.Contains(err.Error(), "regex") || !strings.Contains(err.Error(), "glob") {
		t.Errorf("error should name both modes, got: %v", err)
	}
	// Neither: everything under the scope.
	_, out, err := FindFiles(ctx, nil, FindInput{})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(out.Paths) != fmt.Sprint([]string{"a.md", "b.txt"}) {
		t.Errorf("no pattern should list everything, got %v", out.Paths)
	}
}

// A regex matches against the filename.
func TestFindRegex(t *testing.T) {
	requireFd(t)
	ctx := searchVault(t, map[string]string{"note.md": "", "b.txt": "", "sub/other.md": ""})
	_, out, err := FindFiles(ctx, nil, FindInput{Regex: `^note\.`})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(out.Paths) != fmt.Sprint([]string{"note.md"}) {
		t.Errorf("regex ^note\\.: got %v", out.Paths)
	}
}
