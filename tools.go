package main

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxFileBytes caps how much read_file will return, to avoid flooding the client.
const maxFileBytes = 1 << 20 // 1 MiB

// textOnly wraps a value as the single text result the client displays, while
// also returning the typed payload for structured consumption.
func textResult(format string, a ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, a...)}},
	}
}

// ---- list_files ----

type ListInput struct {
	Path      string `json:"path,omitempty" jsonschema:"folder to list, relative to the served root; empty means the root"`
	Recursive bool   `json:"recursive,omitempty" jsonschema:"if true, walk subfolders recursively"`
}

type Entry struct {
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

type ListOutput struct {
	Entries []Entry `json:"entries"`
}

func ListFiles(ctx context.Context, req *mcp.CallToolRequest, in ListInput) (*mcp.CallToolResult, ListOutput, error) {
	dir, err := resolve(in.Path)
	if err != nil {
		return nil, ListOutput{}, err
	}

	var entries []Entry
	if in.Recursive {
		err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == dir {
				return nil
			}
			info, _ := d.Info()
			var size int64
			if info != nil {
				size = info.Size()
			}
			entries = append(entries, Entry{Path: relPath(p), IsDir: d.IsDir(), Size: size})
			return nil
		})
	} else {
		var des []os.DirEntry
		des, err = os.ReadDir(dir)
		for _, d := range des {
			info, _ := d.Info()
			var size int64
			if info != nil {
				size = info.Size()
			}
			entries = append(entries, Entry{Path: relPath(filepath.Join(dir, d.Name())), IsDir: d.IsDir(), Size: size})
		}
	}
	if err != nil {
		return nil, ListOutput{}, err
	}

	var b strings.Builder
	for _, e := range entries {
		marker := ""
		if e.IsDir {
			marker = "/"
		}
		fmt.Fprintf(&b, "%s%s\n", e.Path, marker)
	}
	if b.Len() == 0 {
		b.WriteString("(empty)\n")
	}
	return textResult("%s", b.String()), ListOutput{Entries: entries}, nil
}

// ---- search ----

type SearchInput struct {
	Query      string `json:"query" jsonschema:"case-insensitive substring to search for"`
	Path       string `json:"path,omitempty" jsonschema:"folder to scope the search to, relative to root; empty means the whole root"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"maximum number of matches to return (default 100)"`
}

type Match struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type SearchOutput struct {
	Matches   []Match `json:"matches"`
	Truncated bool    `json:"truncated"`
}

func Search(ctx context.Context, req *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, SearchOutput{}, fmt.Errorf("query must not be empty")
	}
	scope, err := resolve(in.Path)
	if err != nil {
		return nil, SearchOutput{}, err
	}
	limit := in.MaxResults
	if limit <= 0 {
		limit = 100
	}
	needle := strings.ToLower(in.Query)

	var out SearchOutput
	walkErr := filepath.WalkDir(scope, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than aborting
		}
		if d.IsDir() {
			return nil
		}
		if len(out.Matches) >= limit {
			out.Truncated = true
			return filepath.SkipAll
		}
		matchFile(ctx, p, needle, limit, &out)
		return nil
	})
	if walkErr != nil {
		return nil, SearchOutput{}, walkErr
	}

	var b strings.Builder
	for _, m := range out.Matches {
		fmt.Fprintf(&b, "%s:%d: %s\n", m.Path, m.Line, m.Text)
	}
	if b.Len() == 0 {
		b.WriteString("(no matches)\n")
	}
	if out.Truncated {
		fmt.Fprintf(&b, "... (truncated at %d matches)\n", limit)
	}
	return textResult("%s", b.String()), out, nil
}

// matchFile scans a single file for needle, appending matches to out until limit.
func matchFile(ctx context.Context, path, needle string, limit int, out *SearchOutput) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	// Peek for binary content before scanning lines.
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	if looksBinary(buf[:n]) {
		return
	}
	if _, err := f.Seek(0, 0); err != nil {
		return
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if strings.Contains(strings.ToLower(line), needle) {
			out.Matches = append(out.Matches, Match{Path: relPath(path), Line: lineNo, Text: strings.TrimSpace(line)})
			if len(out.Matches) >= limit {
				out.Truncated = true
				return
			}
		}
	}
}

// ---- read_lines ----

type ReadLinesInput struct {
	Path  string `json:"path" jsonschema:"file to read, relative to root"`
	Start int    `json:"start,omitempty" jsonschema:"first line to read, 1-based inclusive (default 1)"`
	End   int    `json:"end,omitempty" jsonschema:"last line to read, 1-based inclusive; 0 or less means to end of file"`
}

type ReadLinesOutput struct {
	Path    string `json:"path"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
	Content string `json:"content"`
}

func ReadLines(ctx context.Context, req *mcp.CallToolRequest, in ReadLinesInput) (*mcp.CallToolResult, ReadLinesOutput, error) {
	path, err := resolve(in.Path)
	if err != nil {
		return nil, ReadLinesOutput{}, err
	}
	if in.Start < 1 {
		in.Start = 1
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ReadLinesOutput{}, err
	}
	defer f.Close()

	head := make([]byte, 512)
	n, _ := f.Read(head)
	if looksBinary(head[:n]) {
		return nil, ReadLinesOutput{}, fmt.Errorf("%s appears to be a binary file", in.Path)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, ReadLinesOutput{}, err
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var b strings.Builder
	lineNo := 0
	lastEmitted := 0
	for sc.Scan() {
		lineNo++
		if lineNo < in.Start {
			continue
		}
		if in.End > 0 && lineNo > in.End {
			break
		}
		fmt.Fprintf(&b, "%d\t%s\n", lineNo, sc.Text())
		lastEmitted = lineNo
	}
	if err := sc.Err(); err != nil {
		return nil, ReadLinesOutput{}, err
	}

	content := b.String()
	if content == "" {
		content = fmt.Sprintf("(no lines in range; file has %d lines)\n", lineNo)
	}
	end := in.End
	if end <= 0 || end > lastEmitted {
		end = lastEmitted
	}
	return textResult("%s", content), ReadLinesOutput{Path: relPath(path), Start: in.Start, End: end, Content: content}, nil
}

// ---- read_file ----

type ReadFileInput struct {
	Path string `json:"path" jsonschema:"file to read in full, relative to root"`
}

type ReadFileOutput struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Bytes    int    `json:"bytes"`
	Truncated bool  `json:"truncated"`
}

func ReadFile(ctx context.Context, req *mcp.CallToolRequest, in ReadFileInput) (*mcp.CallToolResult, ReadFileOutput, error) {
	path, err := resolve(in.Path)
	if err != nil {
		return nil, ReadFileOutput{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, ReadFileOutput{}, err
	}
	if looksBinary(data[:min(len(data), 512)]) {
		return nil, ReadFileOutput{}, fmt.Errorf("%s appears to be a binary file", in.Path)
	}
	truncated := false
	if len(data) > maxFileBytes {
		data = data[:maxFileBytes]
		truncated = true
	}
	out := ReadFileOutput{Path: relPath(path), Content: string(data), Bytes: len(data), Truncated: truncated}
	text := out.Content
	if truncated {
		text += fmt.Sprintf("\n... (truncated at %d bytes)\n", maxFileBytes)
	}
	return textResult("%s", text), out, nil
}

// ---- write_file ----

type WriteFileInput struct {
	Path    string `json:"path" jsonschema:"file to write, relative to root; parent folders are created as needed"`
	Content string `json:"content" jsonschema:"full new contents of the file"`
	DryRun  bool   `json:"dry_run,omitempty" jsonschema:"if true, return the diff without writing anything"`
}

type WriteFileOutput struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
	Bytes   int    `json:"bytes"`
	DryRun  bool   `json:"dry_run"`
	Diff    string `json:"diff"`
}

func WriteFile(ctx context.Context, req *mcp.CallToolRequest, in WriteFileInput) (*mcp.CallToolResult, WriteFileOutput, error) {
	path, err := resolve(in.Path)
	if err != nil {
		return nil, WriteFileOutput{}, err
	}

	var old string
	created := true
	if info, statErr := os.Stat(path); statErr == nil {
		if info.IsDir() {
			return nil, WriteFileOutput{}, fmt.Errorf("%s is a directory", in.Path)
		}
		created = false
		if data, rdErr := os.ReadFile(path); rdErr == nil && !looksBinary(data[:min(len(data), 512)]) {
			old = string(data)
		}
	}

	diff := diffText(old, in.Content, 3)

	out := WriteFileOutput{Path: relPath(path), Created: created, Bytes: len(in.Content), DryRun: in.DryRun, Diff: diff}
	if in.DryRun {
		return textResult("dry run, no changes written.\n--- diff ---\n%s", diff), out, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, WriteFileOutput{}, err
	}
	if err := os.WriteFile(path, []byte(in.Content), 0o644); err != nil {
		return nil, WriteFileOutput{}, err
	}

	verb := "updated"
	if created {
		verb = "created"
	}
	return textResult("%s %s (%d bytes).\n--- diff ---\n%s", verb, relPath(path), len(in.Content), diff), out, nil
}

// ---- edit_file ----

type EditFileInput struct {
	Path       string `json:"path" jsonschema:"file to edit, relative to root"`
	OldString  string `json:"old_string" jsonschema:"exact text to replace; must occur exactly once unless replace_all is set"`
	NewString  string `json:"new_string" jsonschema:"text to replace it with"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"replace every occurrence instead of requiring a unique match"`
	DryRun     bool   `json:"dry_run,omitempty" jsonschema:"if true, return the diff without writing anything"`
}

type EditFileOutput struct {
	Path         string `json:"path"`
	Replacements int    `json:"replacements"`
	DryRun       bool   `json:"dry_run"`
	Diff         string `json:"diff"`
}

func EditFile(ctx context.Context, req *mcp.CallToolRequest, in EditFileInput) (*mcp.CallToolResult, EditFileOutput, error) {
	if in.OldString == "" {
		return nil, EditFileOutput{}, fmt.Errorf("old_string must not be empty")
	}
	if in.OldString == in.NewString {
		return nil, EditFileOutput{}, fmt.Errorf("old_string and new_string are identical")
	}
	path, err := resolve(in.Path)
	if err != nil {
		return nil, EditFileOutput{}, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, EditFileOutput{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, EditFileOutput{}, err
	}
	if looksBinary(data[:min(len(data), 512)]) {
		return nil, EditFileOutput{}, fmt.Errorf("%s appears to be a binary file", in.Path)
	}

	old := string(data)
	count := strings.Count(old, in.OldString)
	switch {
	case count == 0:
		return nil, EditFileOutput{}, fmt.Errorf("old_string not found in %s", in.Path)
	case count > 1 && !in.ReplaceAll:
		return nil, EditFileOutput{}, fmt.Errorf("old_string occurs %d times in %s; add surrounding context to make it unique, or set replace_all", count, in.Path)
	}

	var updated string
	if in.ReplaceAll {
		updated = strings.ReplaceAll(old, in.OldString, in.NewString)
	} else {
		updated = strings.Replace(old, in.OldString, in.NewString, 1)
		count = 1
	}

	diff := diffText(old, updated, 3)
	out := EditFileOutput{Path: relPath(path), Replacements: count, DryRun: in.DryRun, Diff: diff}
	if in.DryRun {
		return textResult("dry run, no changes written.\n--- diff ---\n%s", diff), out, nil
	}

	if err := os.WriteFile(path, []byte(updated), info.Mode().Perm()); err != nil {
		return nil, EditFileOutput{}, err
	}
	return textResult("edited %s (%d replacement(s)).\n--- diff ---\n%s", relPath(path), count, diff), out, nil
}
