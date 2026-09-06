package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFiles(t *testing.T, ctx context.Context, in ReadFileInput) ReadFileOutput {
	t.Helper()
	_, out, err := ReadFile(ctx, nil, in)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	return out
}

// One entry per path, in order, duplicates preserved.
func TestReadFileBatchOrder(t *testing.T) {
	ctx := searchVault(t, map[string]string{
		"a.md": "alpha\n", "b.md": "beta\n", "sub/c.md": "gamma\n",
	})
	paths := []string{"sub/c.md", "a.md", "sub/c.md", "b.md"}
	out := readFiles(t, ctx, ReadFileInput{Paths: paths})
	if len(out.Entries) != len(paths) {
		t.Fatalf("want %d entries, got %d", len(paths), len(out.Entries))
	}
	want := []string{"gamma\n", "alpha\n", "gamma\n", "beta\n"}
	for i := range paths {
		if out.Entries[i].Path != paths[i] || out.Entries[i].Content != want[i] {
			t.Fatalf("entry %d: %+v", i, out.Entries[i])
		}
	}
}

// A bad path is reported in its entry, not by failing the whole batch — a
// binary file included, which used to be a hard error.
func TestReadFilePerEntryErrors(t *testing.T) {
	ctx := searchVault(t, map[string]string{"a.md": "alpha\n"})
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := readFiles(t, ctx, ReadFileInput{Paths: []string{"a.md", "missing.md", "dir", "bin.dat"}})
	if out.Entries[0].Content != "alpha\n" {
		t.Errorf("good file: %+v", out.Entries[0])
	}
	for i, want := range map[int]string{1: "no such file", 2: "directory", 3: "binary"} {
		if !strings.Contains(out.Entries[i].Error, want) {
			t.Errorf("entry %d: want error containing %q, got %+v", i, want, out.Entries[i])
		}
	}
}

// cap bounds each file and never splits a rune.
func TestReadFileCap(t *testing.T) {
	ctx := searchVault(t, map[string]string{"u.md": "瀬戸内国際芸術祭\n"})
	for cap := 1; cap < 12; cap++ {
		e := readFiles(t, ctx, ReadFileInput{Paths: []string{"u.md"}, Cap: cap}).Entries[0]
		if len(e.Content) > cap {
			t.Fatalf("cap %d exceeded: %d bytes", cap, len(e.Content))
		}
		if !e.Truncated {
			t.Fatalf("cap %d: want truncated", cap)
		}
		if strings.ContainsRune(e.Content, '�') {
			t.Fatalf("cap %d split a rune: %q", cap, e.Content)
		}
	}
	// A file that exactly fills the cap is complete, not truncated.
	ctx = searchVault(t, map[string]string{"x.md": "abcde"})
	if e := readFiles(t, ctx, ReadFileInput{Paths: []string{"x.md"}, Cap: 5}).Entries[0]; e.Truncated || e.Content != "abcde" {
		t.Fatalf("exact fit: %+v", e)
	}
}

// The whole call is bounded by maxFileBytes, so batching cannot return more
// than a single read ever could; later paths say why they have no content.
func TestReadFileTotalBudget(t *testing.T) {
	big := strings.Repeat("x", 700*1024)
	ctx := searchVault(t, map[string]string{"one.md": big, "two.md": big, "three.md": "small\n"})
	out := readFiles(t, ctx, ReadFileInput{Paths: []string{"one.md", "two.md", "three.md"}})
	total := 0
	for _, e := range out.Entries {
		total += e.Bytes
	}
	if total > maxFileBytes {
		t.Fatalf("returned %d bytes, over the %d budget", total, maxFileBytes)
	}
	if !out.Truncated {
		t.Error("want the call flagged as truncated")
	}
	if out.Entries[2].Error == "" {
		t.Errorf("third file should say why it was not read: %+v", out.Entries[2])
	}
}

func TestReadFileRejectsEscapesAndEmptyList(t *testing.T) {
	ctx := searchVault(t, map[string]string{"a.md": "alpha\n"})
	if _, _, err := ReadFile(ctx, nil, ReadFileInput{}); err == nil {
		t.Error("empty paths: want an error")
	}
	for _, p := range []string{"../outside.md", "/etc/passwd", ".git/config"} {
		if _, _, err := ReadFile(ctx, nil, ReadFileInput{Paths: []string{"a.md", p}}); err == nil {
			t.Errorf("%s: want an error", p)
		}
	}
}
