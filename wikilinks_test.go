package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vaultWith writes the given files under a fresh root and returns it.
func vaultWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
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
	return dir
}

// The grammar from the README: a slash decides whether a target is a name or a
// path, and each form resolves from a defined base.
func TestResolveTargetGrammar(t *testing.T) {
	vaultWith(t, map[string]string{
		"tax/README.md":            "the tax folder\n",
		"tax/2026-q3/invoice.md":   "an invoice\n",
		"architecture/README.md":   "the architecture folder\n",
		"concepts/unique-note.md":  "only one of these\n",
		"mails/gmail_123_thing.md": "a mail\n",
	})
	idx, err := buildIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, raw, from, want string
	}{
		{"name finds the sibling first", "README", "tax/2026-q3/invoice.md", ""},
		{"name finds the sibling", "README", "tax/notes.md", "tax/README.md"},
		{"name falls back to the vault", "unique-note", "tax/README.md", "concepts/unique-note.md"},
		{"name is case-insensitive", "Unique-Note", "tax/README.md", "concepts/unique-note.md"},
		{"name with .md", "unique-note.md", "tax/README.md", "concepts/unique-note.md"},
		{"a slash is a vault path", "mails/gmail_123_thing", "tax/2026-q3/invoice.md", "mails/gmail_123_thing.md"},
		{"leading slash is the same", "/mails/gmail_123_thing", "tax/2026-q3/invoice.md", "mails/gmail_123_thing.md"},
		{"./ is explicitly relative", "./README", "tax/notes.md", "tax/README.md"},
		{"alias is stripped", "unique-note|Some Label", "tax/README.md", "concepts/unique-note.md"},
		{"heading is stripped", "unique-note#Section", "tax/README.md", "concepts/unique-note.md"},
		{"missing path does not resolve", "mails/nope", "tax/notes.md", ""},
		{"parent segments never resolve", "../secrets", "tax/notes.md", ""},
		{"escapes never resolve", "../../etc/hostname", "tax/notes.md", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveTarget(tc.raw, tc.from, idx)
			if got.Path != tc.want {
				t.Errorf("resolveTarget(%q, from %q) = %q (reason %q), want %q",
					tc.raw, tc.from, got.Path, got.Reason, tc.want)
			}
		})
	}
}

// The sibling-first rule is what makes a per-folder README convention work: the
// same link text resolves to the README beside the note that wrote it.
func TestNameResolvesToTheNearestReadme(t *testing.T) {
	vaultWith(t, map[string]string{
		"tax/README.md":          "tax\n",
		"tax/notes.md":           "see [[README]]\n",
		"architecture/README.md": "architecture\n",
		"architecture/notes.md":  "see [[README]]\n",
	})
	ctx := context.Background()

	for note, want := range map[string]string{
		"tax/notes.md":          "tax/README.md",
		"architecture/notes.md": "architecture/README.md",
	} {
		out, err := outgoingCore(ctx, note)
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Links) != 1 || out.Links[0].Resolved != want {
			t.Errorf("%s: got %+v, want a link to %s", note, out.Links, want)
		}
	}
}

// An ambiguous name is reported, not guessed: with a README in every folder,
// picking the first candidate would usually be wrong.
func TestAmbiguousNameReportsCandidates(t *testing.T) {
	vaultWith(t, map[string]string{
		"tax/README.md":          "tax\n",
		"architecture/README.md": "architecture\n",
		"top.md":                 "see [[README]]\n",
	})
	out, err := outgoingCore(context.Background(), "top.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Links) != 1 {
		t.Fatalf("want 1 link, got %+v", out.Links)
	}
	l := out.Links[0]
	if l.Resolved != "" || !l.Broken {
		t.Errorf("ambiguous name should not resolve: %+v", l)
	}
	if len(l.Candidates) != 2 {
		t.Errorf("want both candidates reported, got %v", l.Candidates)
	}
	if l.Reason == "" {
		t.Errorf("ambiguity should carry a reason")
	}
}

// Links inside code are examples, not links.
func TestLinksInCodeAreNotLinks(t *testing.T) {
	vaultWith(t, map[string]string{
		"target.md": "x\n",
		"note.md": "real [[target]]\n\n" +
			"inline `[[target]]` example\n\n" +
			"```\n[[target]]\n```\n",
	})
	out, err := outgoingCore(context.Background(), "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Links) != 1 || out.Links[0].Line != 1 {
		t.Errorf("only the line-1 link is real, got %+v", out.Links)
	}
}

// Backlinks follow resolution, not name equality: a link that names this
// basename but resolves elsewhere is not a backlink.
func TestBacklinksFollowResolution(t *testing.T) {
	vaultWith(t, map[string]string{
		"tax/README.md":          "tax\n",
		"architecture/README.md": "architecture\n",
		"tax/notes.md":           "see [[README]]\n",
		"far.md":                 "see [[/tax/README]]\n",
	})
	_, out, err := Backlinks(context.Background(), nil, BacklinksInput{Path: "tax/README.md"})
	if err != nil {
		t.Fatal(err)
	}
	var from []string
	for _, b := range out.Backlinks {
		from = append(from, b.Path)
	}
	got := strings.Join(from, ",")
	if !strings.Contains(got, "tax/notes.md") {
		t.Errorf("sibling link should be a backlink: %v", from)
	}
	if !strings.Contains(got, "far.md") {
		t.Errorf("path link should be a backlink: %v", from)
	}
	if len(from) != 2 {
		t.Errorf("architecture/README.md's own name should not pull in others: %v", from)
	}
}
