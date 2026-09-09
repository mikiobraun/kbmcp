package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	Sort      string `json:"sort,omitempty" jsonschema:"sort order: 'path' (default) or 'modified' (by last-change time)"`
	Reverse   bool   `json:"sort_reverse,omitempty" jsonschema:"reverse the sort order; e.g. sort=modified + sort_reverse=true lists newest first"`
	// Pagination is stateless: From is an exclusive lower bound on the sort key,
	// so pass the previous page's next_from to continue. For sort=path the key is
	// the path; for sort=modified it is an opaque cursor (do not construct it).
	From       string `json:"from,omitempty" jsonschema:"pagination cursor: pass the previous page's next_from to get the next page"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"maximum entries to return (default 200, capped at 1000)"`
}

type Entry struct {
	Path     string `json:"path"`
	IsDir    bool   `json:"is_dir"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"` // last-change time, RFC3339 UTC
}

type ListOutput struct {
	Entries []Entry `json:"entries"`
	// NextFrom is the cursor to pass as From for the next page; empty when this
	// page is the last one. Truncated says whether more entries remain.
	NextFrom  string `json:"next_from,omitempty"`
	Truncated bool   `json:"truncated"`
}

// entrySortKey is the value entries are ordered and paged by. For "modified" the
// key is the mod time (RFC3339 sorts chronologically) with the path appended as
// a tiebreaker, so the key is a total order and the pagination cursor is stable
// even when several files share a timestamp.
func entrySortKey(e Entry, sortBy string) string {
	if sortBy == "modified" {
		return e.Modified + "\t" + e.Path
	}
	return e.Path
}

func ListFiles(ctx context.Context, req *mcp.CallToolRequest, in ListInput) (*mcp.CallToolResult, ListOutput, error) {
	dir, err := resolve(in.Path)
	if err != nil {
		return nil, ListOutput{}, err
	}

	entryFor := func(rel string, d fs.DirEntry) Entry {
		var size int64
		var mod string
		if info, ierr := d.Info(); ierr == nil {
			size = info.Size()
			mod = info.ModTime().UTC().Format(time.RFC3339)
		}
		return Entry{Path: rel, IsDir: d.IsDir(), Size: size, Modified: mod}
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
			if isHidden(d.Name()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			entries = append(entries, entryFor(relPath(p), d))
			return nil
		})
	} else {
		var des []os.DirEntry
		des, err = os.ReadDir(dir)
		for _, d := range des {
			if isHidden(d.Name()) {
				continue
			}
			entries = append(entries, entryFor(relPath(filepath.Join(dir, d.Name())), d))
		}
	}
	if err != nil {
		return nil, ListOutput{}, err
	}

	// Order by the chosen sort key, reversing if asked, then take the page after
	// the cursor. In ascending order the next page is keys > From; in reversed
	// (descending) order it is keys < From — both monotonic, so sort.Search works.
	key := func(e Entry) string { return entrySortKey(e, in.Sort) }
	sort.Slice(entries, func(i, j int) bool { return key(entries[i]) < key(entries[j]) })
	if in.Reverse {
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
	}
	start := 0
	if in.From != "" {
		if in.Reverse {
			start = sort.Search(len(entries), func(i int) bool { return key(entries[i]) < in.From })
		} else {
			start = sort.Search(len(entries), func(i int) bool { return key(entries[i]) > in.From })
		}
	}
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
		out.NextFrom = key(page[len(page)-1])
	}
	out.Entries = page

	var b strings.Builder
	for _, e := range page {
		marker := ""
		if e.IsDir {
			marker = "/"
		}
		if in.Sort == "modified" {
			fmt.Fprintf(&b, "%s%s\t%s\n", e.Path, marker, e.Modified)
		} else {
			fmt.Fprintf(&b, "%s%s\n", e.Path, marker)
		}
	}
	if b.Len() == 0 {
		b.WriteString("(empty)\n")
	}
	if out.Truncated {
		fmt.Fprintf(&b, "... (%d shown; more remain — call again with from=%q)\n", len(page), out.NextFrom)
	}
	return textResult("%s", b.String()), out, nil
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

// maxReadPaths bounds how many files one call may ask for, mirroring
// read_frontmatter. The response size is bounded separately, by maxFileBytes
// across the whole call.
const maxReadPaths = maxListMax

type ReadFileInput struct {
	Paths []string `json:"paths" jsonschema:"files to read in full, relative to root; the output has one entry per path, in this order"`
	Cap   int      `json:"cap,omitempty" jsonschema:"maximum bytes returned per file (default and maximum 1 MiB); a longer file comes back with truncated=true"`
}

type FileEntry struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
	// Error explains why a file has no content — missing, a directory, binary,
	// or past the call's byte budget. Per-entry, so one bad path does not cost
	// the caller the rest of the batch.
	Error string `json:"error,omitempty"`
}

type ReadFileOutput struct {
	Entries []FileEntry `json:"entries"`
	// Truncated is set when any entry was cut short or skipped for budget.
	Truncated bool `json:"truncated"`
}

// readOneFile reads one already-resolved file, returning at most budget bytes.
// Content is capped twice over: by the caller's per-file cap and by what is left
// of the call's overall budget, so a batch can never return more than a single
// read_file used to.
func readOneFile(rel, abs string, limit, budget int) FileEntry {
	e := FileEntry{Path: rel}
	if budget <= 0 {
		e.Error = fmt.Sprintf("not read: the call's %d-byte budget was already used by earlier paths", maxFileBytes)
		e.Truncated = true
		return e
	}
	info, err := os.Stat(abs)
	switch {
	case os.IsNotExist(err):
		e.Error = "no such file"
		return e
	case err != nil:
		e.Error = "cannot read: " + errText(err)
		return e
	case info.IsDir():
		e.Error = "is a directory"
		return e
	}
	if limit > budget {
		limit = budget
	}
	f, err := os.Open(abs)
	if err != nil {
		e.Error = "cannot read: " + errText(err)
		return e
	}
	defer f.Close()
	// One byte past the limit, so a file that exactly fills it is not mislabelled
	// as truncated.
	buf := make([]byte, limit+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		e.Error = "cannot read: " + errText(err)
		return e
	}
	data := buf[:n]
	if looksBinary(data[:min(len(data), 512)]) {
		e.Error = "appears to be a binary file"
		return e
	}
	content := string(data)
	if len(content) > limit {
		// truncateUTF8 backs off to a rune boundary, so a cut file is still
		// valid text rather than ending in half a character.
		content = truncateUTF8(content, limit)
		e.Truncated = true
	}
	e.Content = content
	e.Bytes = len(content)
	return e
}

func ReadFile(ctx context.Context, req *mcp.CallToolRequest, in ReadFileInput) (*mcp.CallToolResult, ReadFileOutput, error) {
	if len(in.Paths) == 0 {
		return nil, ReadFileOutput{}, fmt.Errorf("paths must not be empty")
	}
	if len(in.Paths) > maxReadPaths {
		return nil, ReadFileOutput{}, fmt.Errorf("too many paths: %d (max %d); split the call", len(in.Paths), maxReadPaths)
	}
	limit := in.Cap
	if limit <= 0 || limit > maxFileBytes {
		limit = maxFileBytes
	}
	// Resolve everything first and fail the call on an escape: unlike a missing
	// file, that means the caller asked for something it may not have.
	abs := make([]string, len(in.Paths))
	for i, p := range in.Paths {
		a, err := resolve(p)
		if err != nil {
			return nil, ReadFileOutput{}, err
		}
		abs[i] = a
	}

	// One call returns at most maxFileBytes in total — the same ceiling a single
	// read_file had before it took a list, so batching cannot flood a context
	// window by accident.
	budget := maxFileBytes
	out := ReadFileOutput{Entries: make([]FileEntry, 0, len(in.Paths))}
	var b strings.Builder
	for i, p := range in.Paths {
		e := readOneFile(p, abs[i], limit, budget)
		budget -= e.Bytes
		if e.Truncated {
			out.Truncated = true
		}
		out.Entries = append(out.Entries, e)

		b.WriteString(e.Path)
		b.WriteByte('\n')
		switch {
		case e.Error != "":
			fmt.Fprintf(&b, "(%s)\n", e.Error)
		default:
			b.WriteString(e.Content)
			if !strings.HasSuffix(e.Content, "\n") {
				b.WriteByte('\n')
			}
			if e.Truncated {
				fmt.Fprintf(&b, "... (truncated at %d bytes)\n", len(e.Content))
			}
		}
		b.WriteByte('\n')
	}
	return textResult("%s", b.String()), out, nil
}

// ---- write_file ----

type WriteFileInput struct {
	Path        string `json:"path" jsonschema:"file to write, relative to root; parent folders are created as needed"`
	Content     string `json:"content" jsonschema:"full new contents of the file"`
	Message     string `json:"message" jsonschema:"commit message (required); the file is committed after being written"`
	AuthorName  string `json:"author_name,omitempty" jsonschema:"name to attribute the commit to; defaults to the part of author_email before the @"`
	AuthorEmail string `json:"author_email" jsonschema:"email to attribute the commit to (required): who or what is making this change, e.g. the agent's own address. Every commit names an author, so an automated writer stays distinguishable from a person"`
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

	authorName, authorEmail, err := toolAuthor(in.AuthorName, in.AuthorEmail)
	if err != nil {
		return nil, WriteFileOutput{}, err
	}

	res, err := writeAndCommit(in.Path, in.Content, in.Message, authorName, authorEmail)
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
	AuthorName  string `json:"author_name,omitempty" jsonschema:"name to attribute the commit to; defaults to the part of author_email before the @"`
	AuthorEmail string `json:"author_email" jsonschema:"email to attribute the commit to (required): who or what is making this change, e.g. the agent's own address. Every commit names an author, so an automated writer stays distinguishable from a person"`
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

	authorName, authorEmail, err := toolAuthor(in.AuthorName, in.AuthorEmail)
	if err != nil {
		return nil, EditFileOutput{}, err
	}

	if err := os.WriteFile(path, []byte(updated), info.Mode().Perm()); err != nil {
		return nil, EditFileOutput{}, err
	}

	committed, err := gitCommit([]string{path}, in.Message, authorName, authorEmail)
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
