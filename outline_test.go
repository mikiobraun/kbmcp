package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func outlineOf(t *testing.T, ctx context.Context, path string) OutlineEntry {
	t.Helper()
	_, out, err := ReadOutline(ctx, nil, ReadOutlineInput{Paths: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(out.Entries))
	}
	return out.Entries[0]
}

// A section runs to the line before the next heading of the same or a higher
// level, so it holds its subsections; the last ones run to the end of the file.
func TestReadOutlineRanges(t *testing.T) {
	ctx := searchVault(t, map[string]string{"n.md": strings.Join([]string{
		"preamble",   // 1
		"# Title",    // 2
		"intro",      // 3
		"## Auth",    // 4
		"### Tokens", // 5
		"text",       // 6
		"## Storage", // 7
		"text",       // 8
		"# Appendix", // 9
		"end",        // 10
	}, "\n") + "\n"})

	e := outlineOf(t, ctx, "n.md")
	want := []Heading{
		{Level: 1, Text: "Title", Line: 2, EndLine: 8},
		{Level: 2, Text: "Auth", Line: 4, EndLine: 6},
		{Level: 3, Text: "Tokens", Line: 5, EndLine: 6},
		{Level: 2, Text: "Storage", Line: 7, EndLine: 8},
		{Level: 1, Text: "Appendix", Line: 9, EndLine: 10},
	}
	if e.Lines != 10 || !reflect.DeepEqual(e.Headings, want) {
		t.Errorf("lines=%d headings=%+v", e.Lines, e.Headings)
	}
}

// The ranges are meant for read_lines, so both must count lines the same way.
func TestReadOutlineRangesFeedReadLines(t *testing.T) {
	ctx := searchVault(t, map[string]string{"n.md": "---\ntitle: x\n---\n# A\none\n## B\ntwo\n# C\nthree"})
	e := outlineOf(t, ctx, "n.md")
	_, all, err := ReadLines(ctx, nil, ReadLinesInput{Path: "n.md"})
	if err != nil {
		t.Fatal(err)
	}
	if all.End != e.Lines {
		t.Errorf("read_lines ends at %d, outline says %d lines", all.End, e.Lines)
	}
	for _, h := range e.Headings {
		_, sec, err := ReadLines(ctx, nil, ReadLinesInput{Path: "n.md", Start: h.Line, End: h.EndLine})
		if err != nil {
			t.Fatal(err)
		}
		first := strings.SplitN(sec.Content, "\n", 2)[0]
		if !strings.HasSuffix(first, strings.Repeat("#", h.Level)+" "+h.Text) {
			t.Errorf("section %q starts with %q", h.Text, first)
		}
	}
}

// Not headings: a YAML comment in frontmatter, anything inside a fence, a #tag,
// an indented code line. A #### is not listed and not a boundary either.
func TestReadOutlineSkipsWhatIsNotAHeading(t *testing.T) {
	ctx := searchVault(t, map[string]string{"n.md": strings.Join([]string{
		"---",
		"# yaml comment",
		"tags: [a]",
		"---",
		"#tag at the start of a line",
		"    # indented code",
		"```sh",
		"# shell comment",
		"```",
		"~~~",
		"## tilde fence",
		"~~~",
		"## Real",
		"#### Deep",
		"text",
	}, "\n")})

	e := outlineOf(t, ctx, "n.md")
	want := []Heading{{Level: 2, Text: "Real", Line: 13, EndLine: 15}}
	if !reflect.DeepEqual(e.Headings, want) {
		t.Errorf("headings = %+v", e.Headings)
	}
}

// An opening '---' that is never closed is a rule, not a header hiding the note.
func TestReadOutlineUnclosedFrontmatterIsNotFrontmatter(t *testing.T) {
	ctx := searchVault(t, map[string]string{"n.md": "---\n# Heading\ntext\n"})
	e := outlineOf(t, ctx, "n.md")
	if len(e.Headings) != 1 || e.Headings[0].Line != 2 {
		t.Errorf("headings = %+v", e.Headings)
	}
}

func TestReadOutlineHeadingText(t *testing.T) {
	ctx := searchVault(t, map[string]string{"n.md": "# Closed ##\r\n## C#\r\n###\r\n  # Indented  \r\n"})
	e := outlineOf(t, ctx, "n.md")
	var got []string
	for _, h := range e.Headings {
		got = append(got, h.Text)
	}
	if want := []string{"Closed", "C#", "", "Indented"}; !reflect.DeepEqual(got, want) {
		t.Errorf("texts = %q, want %q", got, want)
	}
}

// One entry per path, in order; a bad file is reported in its entry without
// failing the batch, and a note without headings is not an error.
func TestReadOutlineBatchOrderAndErrors(t *testing.T) {
	ctx := searchVault(t, map[string]string{
		"a.md":     "# A\n",
		"plain.md": "no headings\n",
		"sub/x.md": "# X\n",
		"empty.md": "",
	})
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte{'#', ' ', 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	paths := []string{"plain.md", "missing.md", "a.md", "sub", "bin.dat", "empty.md"}
	_, out, err := ReadOutline(ctx, nil, ReadOutlineInput{Paths: paths})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != len(paths) {
		t.Fatalf("want %d entries, got %d", len(paths), len(out.Entries))
	}
	for i, p := range paths {
		if out.Entries[i].Path != p {
			t.Fatalf("entry %d: path %q, want %q", i, out.Entries[i].Path, p)
		}
	}
	if e := out.Entries[0]; e.Error != "" || e.Headings == nil || len(e.Headings) != 0 || e.Lines != 1 {
		t.Errorf("no headings: %+v", e)
	}
	if out.Entries[1].Error == "" || !strings.Contains(out.Entries[3].Error, "directory") || out.Entries[4].Error == "" {
		t.Errorf("errors: %+v", out.Entries)
	}
	if e := out.Entries[2]; len(e.Headings) != 1 || e.Headings[0].EndLine != 1 {
		t.Errorf("a.md: %+v", e)
	}
	if e := out.Entries[5]; e.Error != "" || e.Lines != 0 {
		t.Errorf("empty file: %+v", e)
	}
}

func TestReadOutlineRejectsEscapesAndBadPathLists(t *testing.T) {
	ctx := searchVault(t, map[string]string{"a.md": "# A\n"})
	for _, p := range []string{"../outside.md", "/etc/passwd", ".git/config"} {
		if _, _, err := ReadOutline(ctx, nil, ReadOutlineInput{Paths: []string{"a.md", p}}); err == nil {
			t.Errorf("%s: want an error", p)
		}
	}
	if _, _, err := ReadOutline(ctx, nil, ReadOutlineInput{}); err == nil {
		t.Error("empty paths: want an error")
	}
	many := make([]string, maxOutlinePaths+1)
	for i := range many {
		many[i] = "a.md"
	}
	if _, _, err := ReadOutline(ctx, nil, ReadOutlineInput{Paths: many}); err == nil {
		t.Error("oversized paths: want an error")
	}
}
