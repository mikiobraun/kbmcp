package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"kbmcp/fmq"
)

// TestMain builds fmq from this tree, so search_frontmatter is tested against
// the fmq it ships with rather than skipped, or run against whatever is on PATH.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kbmcp-fmq")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin := filepath.Join(dir, "fmq")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/fmq").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building fmq: %v\n%s", err, out)
		os.Exit(1)
	}
	fmqCommand = bin
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var fmNotes = map[string]string{
	"mail/a.md":    "---\nsubject: Alpha\nsize: 100\n---\nbody\n",
	"mail/b.md":    "---\nsubject: Beta\nsize: 250\n---\nbody\n",
	"notes/c.md":   "---\ntitle: A note\n---\nbody\n",
	".hidden/d.md": "---\nsubject: Alpha\n---\n",
}

// The semantics are fmq's and tested there; this is the handoff: scope in,
// root-relative paths and the same typed result out.
func TestSearchFrontmatterRunsFmq(t *testing.T) {
	ctx := searchVault(t, fmNotes)
	res, out, err := SearchFrontmatter(ctx, nil, SearchFrontmatterInput{
		Path:    "mail",
		Filters: []fmq.Filter{{Field: "size", Op: "int_gt", Value: "150"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 1 || out.Matches[0].Path != "mail/b.md" || out.Scanned != 2 {
		t.Fatalf("got %+v", out)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "  mail/b.md\n") {
		t.Errorf("text result: %s", text)
	}
}

func TestSearchFrontmatterWholeRoot(t *testing.T) {
	ctx := searchVault(t, fmNotes)
	_, out, err := SearchFrontmatter(ctx, nil, SearchFrontmatterInput{
		Filters: []fmq.Filter{{Field: "subject", Op: "text_eq", Value: "Alpha"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 1 || out.Matches[0].Path != "mail/a.md" {
		t.Fatalf("got %+v", out)
	}
}

// fmq has no limit of its own; kbmcp applies the agent-sized default and cap.
func TestSearchFrontmatterLimits(t *testing.T) {
	files := map[string]string{}
	for i := range defaultListMax + 5 {
		files[fmt.Sprintf("n%04d.md", i)] = "---\nx: 1\n---\n"
	}
	ctx := searchVault(t, files)
	_, out, err := SearchFrontmatter(ctx, nil, SearchFrontmatterInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Matches) != defaultListMax || out.Total != defaultListMax+5 || !out.Truncated {
		t.Fatalf("default: %d of %d, truncated=%v", len(out.Matches), out.Total, out.Truncated)
	}
	three := 3
	if _, out, _ = SearchFrontmatter(ctx, nil, SearchFrontmatterInput{MaxResults: &three}); len(out.Matches) != 3 {
		t.Fatalf("max_results=3: got %d", len(out.Matches))
	}
}

// Fields travel through fmq into both the typed result and the text an agent
// reads.
func TestSearchFrontmatterFields(t *testing.T) {
	ctx := searchVault(t, fmNotes)
	res, out, err := SearchFrontmatter(ctx, nil, SearchFrontmatterInput{
		Path:   "mail",
		Fields: []string{"subject", "missing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(out.Matches[0].Fields); got != "map[missing:[] subject:[Alpha]]" {
		t.Fatalf("fields: %s", got)
	}
	// The text leaves empty fields off each line and states coverage as counts
	// instead, which is also what exposes a misspelt field; the typed result
	// above keeps the empty lists.
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "  mail/a.md  subject: [\"Alpha\"]\n") ||
		!strings.Contains(text, "matches with a value: missing 0/2 · subject 2/2\n") {
		t.Errorf("text result: %s", text)
	}
	// Coverage is over every match, not the page: an agent paging through
	// must not read a field as unused because this page lacks it.
	one := 1
	_, out, err = SearchFrontmatter(ctx, nil, SearchFrontmatterInput{
		Path: "mail", Fields: []string{"size"}, MaxResults: &one, Sort: "path", Reverse: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Matches) != 1 || out.FieldCoverage["size"] != 2 {
		t.Fatalf("coverage over the match set: %+v", out)
	}
}

// fmq's error reaches the agent as fmq wrote it, since it carries the fix.
func TestSearchFrontmatterRelaysErrors(t *testing.T) {
	ctx := searchVault(t, fmNotes)
	_, _, err := SearchFrontmatter(ctx, nil, SearchFrontmatterInput{
		Filters: []fmq.Filter{{Field: "size", Op: "lte", Value: "5"}},
	})
	if err == nil || !strings.HasPrefix(err.Error(), "op \"lte\"") || !strings.Contains(err.Error(), "int_lte") {
		t.Fatalf("got %v", err)
	}
}

// Confinement is kbmcp's job and happens before fmq runs.
func TestSearchFrontmatterScopeIsConfined(t *testing.T) {
	ctx := searchVault(t, fmNotes)
	for _, p := range []string{"../", ".hidden", "/etc"} {
		if _, _, err := SearchFrontmatter(ctx, nil, SearchFrontmatterInput{Path: p}); err == nil {
			t.Errorf("path %q: want an error", p)
		}
	}
}

// A folder named like a flag is a folder, not a flag.
func TestSearchFrontmatterDashFolder(t *testing.T) {
	ctx := searchVault(t, map[string]string{"-n/a.md": "---\nx: y\n---\n"})
	_, out, err := SearchFrontmatter(ctx, nil, SearchFrontmatterInput{Path: "-n"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 1 || out.Matches[0].Path != "-n/a.md" {
		t.Fatalf("got %+v", out)
	}
}
