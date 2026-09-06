package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireRg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep (rg) not installed")
	}
}

// searchVault writes files (map path->content) into a fresh root and returns ctx.
func searchVault(t *testing.T, files map[string]string) context.Context {
	t.Helper()
	dir := t.TempDir()
	for p, c := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := setRoot(dir); err != nil {
		t.Fatal(err)
	}
	return context.Background()
}

func paths(ms []Match) []string {
	var p []string
	for _, m := range ms {
		p = append(p, m.Path)
	}
	return p
}

// Smart case: a lowercase query matches case-insensitively.
func TestSearchSmartCase(t *testing.T) {
	requireRg(t)
	ctx := searchVault(t, map[string]string{
		"a.md": "The Meeting Notes\n",
		"b.md": "no match here\n",
	})
	_, out, err := Search(ctx, nil, SearchInput{Substring: "meeting"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Matches) != 1 || out.Matches[0].Path != "a.md" || out.Matches[0].Line != 1 {
		t.Fatalf("smart-case: %v", out.Matches)
	}
}

// case_sensitive makes an uppercase-only term miss a lowercase line.
func TestSearchCaseSensitive(t *testing.T) {
	requireRg(t)
	ctx := searchVault(t, map[string]string{"a.md": "meeting notes\n"})
	_, out, err := Search(ctx, nil, SearchInput{Substring: "Meeting", Sensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Matches) != 0 {
		t.Fatalf("case-sensitive should miss lowercase: %v", out.Matches)
	}
}

// 'regex' and 'substring' are different matching modes on the same corpus.
func TestSearchRegexAndSubstring(t *testing.T) {
	requireRg(t)
	ctx := searchVault(t, map[string]string{
		"a.md": "from: alice@example.com\n",
		"b.md": "a.b.c literal dots\n",
	})
	// regex: \w+@\w+ matches the email line
	_, re, err := Search(ctx, nil, SearchInput{Regex: `\w+@\w+`})
	if err != nil {
		t.Fatal(err)
	}
	if len(re.Matches) != 1 || re.Matches[0].Path != "a.md" {
		t.Errorf("regex email: %v", re.Matches)
	}
	// substring: "a.b" is literal, so it matches b.md's dots, not "alice" etc.
	_, sub, err := Search(ctx, nil, SearchInput{Substring: "a.b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Matches) != 1 || sub.Matches[0].Path != "b.md" {
		t.Errorf("substring a.b: %v", sub.Matches)
	}
}

// The regression this split exists for: a markdown checkbox is a valid regex
// meaning something else entirely ("- " then a space), so it silently matched
// the wrong line. As a substring it finds the real open todo.
func TestSearchCheckboxLiteral(t *testing.T) {
	requireRg(t)
	ctx := searchVault(t, map[string]string{
		"todo.md": "- [ ] open todo\n- [x] done todo\n-  two spaces\n",
	})
	_, sub, err := Search(ctx, nil, SearchInput{Substring: "- [ ]"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Matches) != 1 || sub.Matches[0].Line != 1 {
		t.Errorf("substring checkbox: %v", sub.Matches)
	}
	// The same text as a regex is a character class, and lands elsewhere.
	_, re, err := Search(ctx, nil, SearchInput{Regex: "- [ ]"})
	if err != nil {
		t.Fatal(err)
	}
	if len(re.Matches) != 1 || re.Matches[0].Line != 3 {
		t.Errorf("regex checkbox: %v", re.Matches)
	}
}

// Exactly one mode must be named: neither and both are errors, and each error
// explains the difference rather than just rejecting the call.
func TestSearchRequiresExactlyOneMode(t *testing.T) {
	requireRg(t)
	ctx := searchVault(t, map[string]string{"a.md": "text\n"})
	for _, in := range []SearchInput{
		{},
		{Regex: "a", Substring: "a"},
	} {
		_, _, err := Search(ctx, nil, in)
		if err == nil {
			t.Fatalf("expected an error for %+v", in)
		}
		if !strings.Contains(err.Error(), "substring") || !strings.Contains(err.Error(), "regex") {
			t.Errorf("error should name both modes, got: %v", err)
		}
	}
}

// glob restricts content search to files whose name matches, and '!' excludes.
func TestSearchGlob(t *testing.T) {
	requireRg(t)
	ctx := searchVault(t, map[string]string{
		"a.md":  "needle here\n",
		"b.txt": "needle here\n",
	})
	_, only, err := Search(ctx, nil, SearchInput{Substring: "needle", Glob: "*.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(only.Matches) != 1 || only.Matches[0].Path != "a.md" {
		t.Errorf("glob include *.md: %v", paths(only.Matches))
	}
	_, excl, err := Search(ctx, nil, SearchInput{Substring: "needle", Glob: "!*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(excl.Matches) != 1 || excl.Matches[0].Path != "a.md" {
		t.Errorf("glob exclude !*.txt: %v", paths(excl.Matches))
	}
}

// path scopes the search to a subtree.
func TestSearchScope(t *testing.T) {
	requireRg(t)
	ctx := searchVault(t, map[string]string{
		"top.md":          "needle\n",
		"drafts/x.md":     "needle\n",
		"drafts/sub/y.md": "needle\n",
	})
	_, out, err := Search(ctx, nil, SearchInput{Substring: "needle", Path: "drafts"})
	if err != nil {
		t.Fatal(err)
	}
	got := paths(out.Matches)
	for _, p := range got {
		if !strings.HasPrefix(p, "drafts/") {
			t.Errorf("scope leaked outside drafts/: %v", got)
		}
	}
	if len(got) != 2 {
		t.Errorf("expected 2 matches under drafts/, got %v", got)
	}
}

// max_results caps the matches and sets Truncated.
func TestSearchTruncation(t *testing.T) {
	requireRg(t)
	files := map[string]string{}
	for i := 0; i < 10; i++ {
		files[fmt.Sprintf("f%02d.md", i)] = "needle\n"
	}
	ctx := searchVault(t, files)
	_, out, err := Search(ctx, nil, SearchInput{Substring: "needle", MaxResults: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Matches) != 4 || !out.Truncated {
		t.Fatalf("expected 4 matches truncated, got %d truncated=%v", len(out.Matches), out.Truncated)
	}
}

// ripgrep skips hidden files and .git by default, so search never reads them.
func TestSearchHidesDotfiles(t *testing.T) {
	requireRg(t)
	ctx := searchVault(t, map[string]string{
		"note.md":     "hello secret\n",
		".env":        "TOKEN=secret\n",
		".git/config": "secret\n",
	})
	_, out, err := Search(ctx, nil, SearchInput{Substring: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Matches) != 1 || out.Matches[0].Path != "note.md" {
		t.Errorf("search should only match the visible note, got %v", paths(out.Matches))
	}
}

// A malformed regex surfaces as an error, not silent emptiness.
func TestSearchBadRegex(t *testing.T) {
	requireRg(t)
	ctx := searchVault(t, map[string]string{"a.md": "x\n"})
	if _, _, err := Search(ctx, nil, SearchInput{Regex: "("}); err == nil {
		t.Error("expected an error for an unterminated group")
	}
}

// The path scope goes through resolve(), so it can't escape the vault — an
// absolute path or a ..-escape is rejected before ripgrep ever runs.
func TestSearchPathConfinement(t *testing.T) {
	requireRg(t)
	ctx := searchVault(t, map[string]string{"note.md": "x\n"})
	for _, bad := range []string{"..", "../..", "../../etc", "/etc", "sub/../.."} {
		if _, _, err := Search(ctx, nil, SearchInput{Substring: "x", Path: bad}); err == nil {
			t.Errorf("path %q should be rejected as escaping the vault", bad)
		}
	}
}
