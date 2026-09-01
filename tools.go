package main

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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

// defaultListMax / maxListMax bound how many entries one list_files call
// returns, so an agent listing a large (e.g. recursively walked) vault gets a
// bounded page instead of the whole tree at once.
const (
	defaultListMax = 200
	maxListMax     = 1000
)

type ListInput struct {
	Path      string `json:"path,omitempty" jsonschema:"folder to list, relative to the served root; empty means the root"`
	Recursive bool   `json:"recursive,omitempty" jsonschema:"if true, walk subfolders recursively"`
	// Pagination. Entries are sorted by path; From is an exclusive lower bound
	// (return only paths that sort strictly after it), so paging is stateless:
	// pass the previous page's next_from to get the next page.
	From       string `json:"from,omitempty" jsonschema:"pagination cursor: only return entries whose path sorts strictly after this string; pass the previous page's next_from to continue"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"maximum entries to return (default 200, capped at 1000)"`
}

type Entry struct {
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

type ListOutput struct {
	Entries []Entry `json:"entries"`
	// NextFrom is the cursor to pass as From for the next page; empty when this
	// page is the last one. Truncated says whether more entries remain.
	NextFrom  string `json:"next_from,omitempty"`
	Truncated bool   `json:"truncated"`
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

	// Sort by path for a stable order, then take the page strictly after From.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	start := sort.Search(len(entries), func(i int) bool { return entries[i].Path > in.From })
	page := entries[start:]

	limit := in.MaxResults
	if limit <= 0 {
		limit = defaultListMax
	}
	if limit > maxListMax {
		limit = maxListMax
	}
	out := ListOutput{}
	if len(page) > limit {
		out.Truncated = true
		page = page[:limit]
		out.NextFrom = page[len(page)-1].Path
	}
	out.Entries = page

	var b strings.Builder
	for _, e := range page {
		marker := ""
		if e.IsDir {
			marker = "/"
		}
		fmt.Fprintf(&b, "%s%s\n", e.Path, marker)
	}
	if b.Len() == 0 {
		b.WriteString("(empty)\n")
	}
	if out.Truncated {
		fmt.Fprintf(&b, "... (%d shown; more remain — call again with from=%q)\n", len(page), out.NextFrom)
	}
	return textResult("%s", b.String()), out, nil
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
	Path        string `json:"path" jsonschema:"file to write, relative to root; parent folders are created as needed"`
	Content     string `json:"content" jsonschema:"full new contents of the file"`
	Message     string `json:"message" jsonschema:"commit message (required); the file is committed after being written"`
	AuthorName  string `json:"author_name,omitempty" jsonschema:"name to attribute the commit to (e.g. the agent making the change); defaults to the repo's configured identity"`
	AuthorEmail string `json:"author_email,omitempty" jsonschema:"email to attribute the commit to; defaults to the repo's configured identity"`
	DryRun      bool   `json:"dry_run,omitempty" jsonschema:"if true, return the diff without writing or committing anything"`
}

type WriteFileOutput struct {
	Path      string `json:"path"`
	Created   bool   `json:"created"`
	Bytes     int    `json:"bytes"`
	DryRun    bool   `json:"dry_run"`
	Committed bool   `json:"committed"`
	Diff      string `json:"diff"`
}

// writeOutcome is the result of writeAndCommit.
type writeOutcome struct {
	Abs       string // resolved absolute path written
	Old       string // previous content (for diffing), empty if created or binary
	Created   bool   // the file did not exist before
	Committed bool   // a commit was actually made
}

// readForWrite returns the current content of an already-resolved path (empty if
// it doesn't exist or is binary) and whether a write there would create a new
// file. It errors if the path is an existing directory.
func readForWrite(abs string) (old string, created bool, err error) {
	info, statErr := os.Stat(abs)
	if statErr != nil {
		return "", true, nil
	}
	if info.IsDir() {
		return "", false, fmt.Errorf("%s is a directory", relPath(abs))
	}
	if data, rdErr := os.ReadFile(abs); rdErr == nil && !looksBinary(data[:min(len(data), 512)]) {
		old = string(data)
	}
	return old, false, nil
}

// writeAndCommit creates or overwrites the file at rel with content, creating
// parent folders, then commits it with message (required) and the optional
// author identity. It is the shared core behind both write_file and the REST
// PUT handler.
func writeAndCommit(rel, content, message, authorName, authorEmail string) (writeOutcome, error) {
	if strings.TrimSpace(message) == "" {
		return writeOutcome{}, fmt.Errorf("message is required")
	}
	abs, err := resolve(rel)
	if err != nil {
		return writeOutcome{}, err
	}
	old, created, err := readForWrite(abs)
	if err != nil {
		return writeOutcome{}, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return writeOutcome{}, err
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return writeOutcome{}, err
	}
	committed, err := gitCommit([]string{abs}, message, authorName, authorEmail)
	if err != nil {
		return writeOutcome{}, err
	}
	return writeOutcome{Abs: abs, Old: old, Created: created, Committed: committed}, nil
}

func WriteFile(ctx context.Context, req *mcp.CallToolRequest, in WriteFileInput) (*mcp.CallToolResult, WriteFileOutput, error) {
	if strings.TrimSpace(in.Message) == "" {
		return nil, WriteFileOutput{}, fmt.Errorf("message is required")
	}
	path, err := resolve(in.Path)
	if err != nil {
		return nil, WriteFileOutput{}, err
	}

	if in.DryRun {
		old, created, err := readForWrite(path)
		if err != nil {
			return nil, WriteFileOutput{}, err
		}
		diff := diffText(old, in.Content, 3)
		out := WriteFileOutput{Path: relPath(path), Created: created, Bytes: len(in.Content), DryRun: true, Diff: diff}
		return textResult("dry run, no changes written.\n--- diff ---\n%s", diff), out, nil
	}

	res, err := writeAndCommit(in.Path, in.Content, in.Message, in.AuthorName, in.AuthorEmail)
	if err != nil {
		return nil, WriteFileOutput{}, err
	}

	diff := diffText(res.Old, in.Content, 3)
	out := WriteFileOutput{Path: relPath(res.Abs), Created: res.Created, Bytes: len(in.Content), Committed: res.Committed, Diff: diff}

	verb := "updated"
	if res.Created {
		verb = "created"
	}
	status := "committed"
	if !res.Committed {
		status = "no changes to commit"
	}
	return textResult("%s %s (%d bytes) — %s.\n--- diff ---\n%s", verb, relPath(res.Abs), len(in.Content), status, diff), out, nil
}

// ---- edit_file ----

type EditFileInput struct {
	Path        string `json:"path" jsonschema:"file to edit, relative to root"`
	OldString   string `json:"old_string" jsonschema:"exact text to replace; must occur exactly once unless replace_all is set"`
	NewString   string `json:"new_string" jsonschema:"text to replace it with"`
	Message     string `json:"message" jsonschema:"commit message (required); the file is committed after being edited"`
	AuthorName  string `json:"author_name,omitempty" jsonschema:"name to attribute the commit to (e.g. the agent making the change); defaults to the repo's configured identity"`
	AuthorEmail string `json:"author_email,omitempty" jsonschema:"email to attribute the commit to; defaults to the repo's configured identity"`
	ReplaceAll  bool   `json:"replace_all,omitempty" jsonschema:"replace every occurrence instead of requiring a unique match"`
	DryRun      bool   `json:"dry_run,omitempty" jsonschema:"if true, return the diff without writing or committing anything"`
}

type EditFileOutput struct {
	Path         string `json:"path"`
	Replacements int    `json:"replacements"`
	DryRun       bool   `json:"dry_run"`
	Committed    bool   `json:"committed"`
	Diff         string `json:"diff"`
}

func EditFile(ctx context.Context, req *mcp.CallToolRequest, in EditFileInput) (*mcp.CallToolResult, EditFileOutput, error) {
	if strings.TrimSpace(in.Message) == "" {
		return nil, EditFileOutput{}, fmt.Errorf("message is required")
	}
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

	committed, err := gitCommit([]string{path}, in.Message, in.AuthorName, in.AuthorEmail)
	if err != nil {
		return nil, EditFileOutput{}, err
	}
	out.Committed = committed

	status := "committed"
	if !committed {
		status = "no changes to commit"
	}
	return textResult("edited %s (%d replacement(s)) — %s.\n--- diff ---\n%s", relPath(path), count, status, diff), out, nil
}
