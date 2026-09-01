package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Caps to keep history output from flooding the client. maxDiffBytes bounds a
// diff payload; defaultLogMax / maxLogMax bound how many commits history returns.
const (
	maxDiffBytes  = 256 << 10 // 256 KiB
	defaultLogMax = 20
	maxLogMax     = 500
)

// logSep is an unlikely-in-text field separator for parsing git log output.
const logSep = "\x1f"

// ---- history ----

type HistoryInput struct {
	Path  string `json:"path,omitempty" jsonschema:"limit history to this file or folder, relative to root; empty means the whole repo. Renames are followed for a single file."`
	Max   int    `json:"max,omitempty" jsonschema:"maximum number of commits to return (default 20)"`
	Since string `json:"since,omitempty" jsonschema:"only commits after this ref (exclusive), e.g. a hash from an earlier history call — use to see what changed since you last looked"`
}

type Commit struct {
	Hash     string `json:"hash"`
	Date     string `json:"date"`     // committer date, ISO 8601
	Relative string `json:"relative"` // e.g. "2 hours ago"
	Author   string `json:"author"`
	Subject  string `json:"subject"`
}

type HistoryOutput struct {
	Commits []Commit `json:"commits"`
}

// commitLog returns recent commits, applying the same max/since/path handling as
// the history tool. Shared by the history MCP tool and the REST /history endpoint.
func commitLog(max int, since, path string) ([]Commit, error) {
	if max <= 0 {
		max = defaultLogMax
	}
	if max > maxLogMax {
		max = maxLogMax
	}

	args := []string{"log", "--no-color", "-M",
		fmt.Sprintf("-n%d", max),
		"--pretty=format:%h" + logSep + "%cI" + logSep + "%cr" + logSep + "%an" + logSep + "%s",
	}

	if since != "" {
		if !validRef(since) {
			return nil, fmt.Errorf("invalid since ref: %q", since)
		}
		args = append(args, since+"..HEAD")
	}

	if path != "" {
		abs, err := resolve(path)
		if err != nil {
			return nil, err
		}
		rel := relPath(abs)
		// --follow tracks renames but only works for a single existing file.
		if info, statErr := os.Stat(abs); statErr == nil && !info.IsDir() {
			args = append(args, "--follow")
		}
		args = append(args, "--", rel)
	}

	out, err := runGit(args...)
	if err != nil {
		return nil, err
	}

	var commits []Commit
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, logSep)
		if len(f) != 5 {
			continue
		}
		commits = append(commits, Commit{Hash: f[0], Date: f[1], Relative: f[2], Author: f[3], Subject: f[4]})
	}
	return commits, nil
}

func History(ctx context.Context, req *mcp.CallToolRequest, in HistoryInput) (*mcp.CallToolResult, HistoryOutput, error) {
	commits, err := commitLog(in.Max, in.Since, in.Path)
	if err != nil {
		return nil, HistoryOutput{}, err
	}

	var b strings.Builder
	for _, c := range commits {
		fmt.Fprintf(&b, "%s  %s  %s  %s\n", c.Hash, c.Relative, c.Author, c.Subject)
	}
	if b.Len() == 0 {
		b.WriteString("(no commits)\n")
	}
	return textResult("%s", b.String()), HistoryOutput{Commits: commits}, nil
}

// ---- diff ----

type DiffInput struct {
	From string `json:"from,omitempty" jsonschema:"base commit ref (default HEAD~1)"`
	To   string `json:"to,omitempty" jsonschema:"target commit ref (default HEAD)"`
	Path string `json:"path,omitempty" jsonschema:"limit the diff to this file or folder, relative to root"`
	Stat bool   `json:"stat,omitempty" jsonschema:"if true, return a compact summary (changed files with insertion/deletion counts) instead of the full patch"`
}

type DiffOutput struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Diff      string `json:"diff"`
	Truncated bool   `json:"truncated"`
}

func Diff(ctx context.Context, req *mcp.CallToolRequest, in DiffInput) (*mcp.CallToolResult, DiffOutput, error) {
	from, to := in.From, in.To
	if from == "" {
		from = "HEAD~1"
	}
	if to == "" {
		to = "HEAD"
	}
	if !validRef(from) {
		return nil, DiffOutput{}, fmt.Errorf("invalid from ref: %q", from)
	}
	if !validRef(to) {
		return nil, DiffOutput{}, fmt.Errorf("invalid to ref: %q", to)
	}

	args := []string{"diff", "--no-color", "-M"}
	if in.Stat {
		args = append(args, "--stat=200")
	}
	args = append(args, from, to)

	if in.Path != "" {
		abs, err := resolve(in.Path)
		if err != nil {
			return nil, DiffOutput{}, err
		}
		args = append(args, "--", relPath(abs))
	}

	out, err := runGit(args...)
	if err != nil {
		return nil, DiffOutput{}, err
	}

	truncated := false
	if len(out) > maxDiffBytes {
		out = out[:maxDiffBytes]
		truncated = true
	}

	res := DiffOutput{From: from, To: to, Diff: out, Truncated: truncated}
	text := out
	if text == "" {
		text = "(no differences)\n"
	}
	if truncated {
		text += fmt.Sprintf("\n... (truncated at %d bytes)\n", maxDiffBytes)
	}
	return textResult("%s", text), res, nil
}

// ---- file_at ----

type FileAtInput struct {
	Path string `json:"path" jsonschema:"file to read, relative to root"`
	Ref  string `json:"ref" jsonschema:"commit ref to read the file at, e.g. a hash from history or HEAD~1"`
}

type FileAtOutput struct {
	Path      string `json:"path"`
	Ref       string `json:"ref"`
	Content   string `json:"content"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
}

func FileAt(ctx context.Context, req *mcp.CallToolRequest, in FileAtInput) (*mcp.CallToolResult, FileAtOutput, error) {
	if !validRef(in.Ref) {
		return nil, FileAtOutput{}, fmt.Errorf("invalid ref: %q", in.Ref)
	}
	abs, err := resolve(in.Path)
	if err != nil {
		return nil, FileAtOutput{}, err
	}
	rel := relPath(abs)

	// git show <ref>:<path> reads the path relative to the repo root.
	out, err := runGit("show", in.Ref+":"+rel)
	if err != nil {
		return nil, FileAtOutput{}, err
	}

	data := []byte(out)
	if looksBinary(data[:min(len(data), 512)]) {
		return nil, FileAtOutput{}, fmt.Errorf("%s at %s appears to be a binary file", rel, in.Ref)
	}
	truncated := false
	if len(data) > maxFileBytes {
		data = data[:maxFileBytes]
		truncated = true
	}

	res := FileAtOutput{Path: rel, Ref: in.Ref, Content: string(data), Bytes: len(data), Truncated: truncated}
	text := res.Content
	if truncated {
		text += fmt.Sprintf("\n... (truncated at %d bytes)\n", maxFileBytes)
	}
	return textResult("%s", text), res, nil
}
