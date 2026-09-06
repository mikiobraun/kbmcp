package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readFM is a one-path shorthand for the common case.
func readFM(t *testing.T, ctx context.Context, path string, cap int) FrontmatterEntry {
	t.Helper()
	_, out, err := ReadFrontmatter(ctx, nil, ReadFrontmatterInput{Paths: []string{path}, Cap: cap})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(out.Entries))
	}
	return out.Entries[0]
}

// The block between the delimiters comes back verbatim, delimiters excluded.
func TestReadFrontmatterVerbatim(t *testing.T) {
	ctx := searchVault(t, map[string]string{
		"a.md": "---\ndate: '2026-08-28T20:54:22+00:00'\ntags: [x, y]\n---\n# Body\n\ntext\n",
	})
	e := readFM(t, ctx, "a.md", 0)
	want := "date: '2026-08-28T20:54:22+00:00'\ntags: [x, y]\n"
	if !e.HasFrontmatter || e.Truncated || e.Frontmatter != want {
		t.Fatalf("got %+v, want frontmatter %q", e, want)
	}
}

// A file that does not open with a delimiter has no frontmatter — including one
// where a '---' appears later, or after a leading blank line.
func TestReadFrontmatterAbsent(t *testing.T) {
	ctx := searchVault(t, map[string]string{
		"plain.md": "# Just a note\n\nno frontmatter here\n",
		"late.md":  "# Note\n---\nnot: frontmatter\n---\n",
		"blank.md": "\n---\ndate: 2026-01-01\n---\n",
		"hrule.md": "---\n",
	})
	for _, p := range []string{"plain.md", "late.md", "blank.md"} {
		if e := readFM(t, ctx, p, 0); e.HasFrontmatter || e.Frontmatter != "" || e.Error != "" {
			t.Errorf("%s: want no frontmatter, got %+v", p, e)
		}
	}
	// A lone '---' opens a block that is never closed: present, but truncated.
	if e := readFM(t, ctx, "hrule.md", 0); !e.HasFrontmatter || !e.Truncated {
		t.Errorf("lone delimiter: got %+v", e)
	}
}

// An empty block is distinguishable from no block at all.
func TestReadFrontmatterEmptyBlock(t *testing.T) {
	ctx := searchVault(t, map[string]string{"e.md": "---\n---\n# Body\n"})
	e := readFM(t, ctx, "e.md", 0)
	if !e.HasFrontmatter || e.Frontmatter != "" || e.Truncated {
		t.Fatalf("got %+v, want has_frontmatter with empty block", e)
	}
}

// Over-cap and never-closed are the same case: what fits, flagged truncated.
func TestReadFrontmatterTruncates(t *testing.T) {
	long := strings.Repeat("k: vvvvvvvvv\n", 50) // 650 bytes
	ctx := searchVault(t, map[string]string{
		"big.md":    "---\n" + long + "---\n# Body\n",
		"unterm.md": "---\n" + long + "# no closing delimiter, ever\n",
	})
	for _, p := range []string{"big.md", "unterm.md"} {
		e := readFM(t, ctx, p, 100)
		if !e.HasFrontmatter || !e.Truncated || len(e.Frontmatter) != 100 {
			t.Errorf("%s: got %d bytes truncated=%v", p, len(e.Frontmatter), e.Truncated)
		}
	}
	// A bigger cap gets the whole block, and the same call is now complete.
	if e := readFM(t, ctx, "big.md", 5000); e.Truncated || e.Frontmatter != long {
		t.Errorf("large cap: truncated=%v len=%d want %d", e.Truncated, len(e.Frontmatter), len(long))
	}
}

// A block ending exactly at the cap is complete, not truncated (off-by-one).
func TestReadFrontmatterCapBoundary(t *testing.T) {
	block := "abcdefghij\n" // 11 bytes
	ctx := searchVault(t, map[string]string{"b.md": "---\n" + block + "---\n"})
	if e := readFM(t, ctx, "b.md", len(block)); e.Truncated || e.Frontmatter != block {
		t.Fatalf("at cap: %+v", e)
	}
	if e := readFM(t, ctx, "b.md", len(block)-1); !e.Truncated {
		t.Fatalf("below cap: want truncated, got %+v", e)
	}
}

// Truncation must not split a multi-byte rune.
func TestReadFrontmatterUTF8Boundary(t *testing.T) {
	ctx := searchVault(t, map[string]string{"u.md": "---\ntitle: 瀬戸内国際芸術祭\n---\n"})
	for cap := 8; cap < 20; cap++ {
		e := readFM(t, ctx, "u.md", cap)
		if !isValidUTF8(e.Frontmatter) {
			t.Fatalf("cap %d produced invalid UTF-8: %q", cap, e.Frontmatter)
		}
		if len(e.Frontmatter) > cap {
			t.Fatalf("cap %d exceeded: %d bytes", cap, len(e.Frontmatter))
		}
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// CRLF files are recognised; the block keeps its original line endings.
func TestReadFrontmatterCRLF(t *testing.T) {
	ctx := searchVault(t, map[string]string{"w.md": "---\r\ndate: 2026-01-01\r\n---\r\n# Body\r\n"})
	e := readFM(t, ctx, "w.md", 0)
	if !e.HasFrontmatter || e.Frontmatter != "date: 2026-01-01\r\n" {
		t.Fatalf("got %+v", e)
	}
}

// One entry per input path, in order, duplicates preserved; a bad file is
// reported in its entry without failing the batch.
func TestReadFrontmatterBatchOrderAndErrors(t *testing.T) {
	ctx := searchVault(t, map[string]string{
		"a.md":  "---\nn: 1\n---\n",
		"b.md":  "---\nn: 2\n---\n",
		"sub/c": "not markdown, no frontmatter",
	})
	paths := []string{"b.md", "missing.md", "a.md", "b.md", "sub"}
	_, out, err := ReadFrontmatter(ctx, nil, ReadFrontmatterInput{Paths: paths})
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
	if out.Entries[0].Frontmatter != "n: 2\n" || out.Entries[3].Frontmatter != "n: 2\n" {
		t.Errorf("duplicate path not repeated: %+v", out.Entries)
	}
	if out.Entries[1].Error == "" {
		t.Errorf("missing file: want an error, got %+v", out.Entries[1])
	}
	if out.Entries[2].Frontmatter != "n: 1\n" {
		t.Errorf("a.md: %+v", out.Entries[2])
	}
	if !strings.Contains(out.Entries[4].Error, "directory") {
		t.Errorf("directory: want an error, got %+v", out.Entries[4])
	}
}

// A binary file is reported per-entry, not returned as garbage.
func TestReadFrontmatterBinary(t *testing.T) {
	ctx := searchVault(t, map[string]string{})
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte{'-', '-', '-', '\n', 0x00, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}
	if e := readFM(t, ctx, "bin.dat", 0); e.Error == "" || e.HasFrontmatter {
		t.Fatalf("binary: %+v", e)
	}
}

// Path escapes fail the whole call: they are a caller bug, not an odd file.
func TestReadFrontmatterRejectsEscape(t *testing.T) {
	ctx := searchVault(t, map[string]string{"a.md": "---\nn: 1\n---\n"})
	for _, p := range []string{"../outside.md", "/etc/passwd", ".git/config"} {
		if _, _, err := ReadFrontmatter(ctx, nil, ReadFrontmatterInput{Paths: []string{"a.md", p}}); err == nil {
			t.Errorf("%s: want an error", p)
		}
	}
}

func TestReadFrontmatterRejectsEmptyAndOversizedPathList(t *testing.T) {
	ctx := searchVault(t, map[string]string{"a.md": "---\nn: 1\n---\n"})
	if _, _, err := ReadFrontmatter(ctx, nil, ReadFrontmatterInput{}); err == nil {
		t.Error("empty paths: want an error")
	}
	many := make([]string, maxFrontmatterPaths+1)
	for i := range many {
		many[i] = "a.md"
	}
	if _, _, err := ReadFrontmatter(ctx, nil, ReadFrontmatterInput{Paths: many}); err == nil {
		t.Error("oversized paths: want an error")
	}
}
