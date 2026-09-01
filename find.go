package main

// find_files: name-based discovery, backed by fd. A separate tool from search
// on purpose — its result shape is a list of paths, not content matches. Like
// ripgrep, fd skips hidden files, .git, and gitignored paths by default, and
// does not follow symlinks unless asked, so the scope stays inside the vault.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type FindInput struct {
	Pattern    string `json:"pattern,omitempty" jsonschema:"filename pattern; a regular expression unless glob is set. Empty matches everything under the scope."`
	Glob       bool   `json:"glob,omitempty" jsonschema:"treat pattern as a glob (e.g. '*.md') instead of a regular expression"`
	Type       string `json:"type,omitempty" jsonschema:"what to find: 'file' (default) or 'dir'"`
	Path       string `json:"path,omitempty" jsonschema:"folder to scope the search to, relative to root; empty means the whole root"`
	From       string `json:"from,omitempty" jsonschema:"pagination cursor: pass the previous page's next_from to get the next page"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"maximum paths to return (default 200, capped at 1000)"`
}

type FindOutput struct {
	Paths     []string `json:"paths"`
	NextFrom  string   `json:"next_from,omitempty"`
	Truncated bool     `json:"truncated"`
}

// fdBinary finds fd, which is packaged as "fdfind" on some distros.
func fdBinary() (string, error) {
	for _, name := range []string{"fd", "fdfind"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("find_files requires fd (or fdfind), which is not installed")
}

func FindFiles(ctx context.Context, req *mcp.CallToolRequest, in FindInput) (*mcp.CallToolResult, FindOutput, error) {
	scope, err := resolve(in.Path)
	if err != nil {
		return nil, FindOutput{}, err
	}
	fd, err := fdBinary()
	if err != nil {
		return nil, FindOutput{}, err
	}

	typ := "f"
	switch in.Type {
	case "", "file", "f":
		typ = "f"
	case "dir", "directory", "d":
		typ = "d"
	default:
		return nil, FindOutput{}, fmt.Errorf("type must be 'file' or 'dir', got %q", in.Type)
	}

	// Run from root with a relative --search-path so paths come back relative to
	// root. The flag form (not a positional) keeps the scope from being mistaken
	// for the pattern.
	args := []string{"--color=never", "--type", typ, "--search-path", relPath(scope)}
	if in.Glob {
		args = append(args, "--glob")
	}
	if strings.TrimSpace(in.Pattern) != "" {
		args = append(args, "--", in.Pattern)
	}

	cmd := exec.CommandContext(ctx, fd, args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, FindOutput{}, err
	}
	if err := cmd.Start(); err != nil {
		return nil, FindOutput{}, err
	}

	var all []string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// --search-path . prefixes "./"; directories carry a trailing "/".
		p := strings.TrimSuffix(strings.TrimPrefix(sc.Text(), "./"), "/")
		if p != "" {
			all = append(all, p)
		}
	}
	// fd exits 0 even with no matches, so any non-nil error here is a real one
	// (e.g. an invalid regex).
	if werr := cmd.Wait(); werr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = werr.Error()
		}
		return nil, FindOutput{}, fmt.Errorf("fd: %s", msg)
	}

	// Sort for a stable order, then take the page strictly after the cursor.
	sort.Strings(all)
	start := sort.Search(len(all), func(i int) bool { return all[i] > in.From })
	page := all[start:]

	limit := in.MaxResults
	if limit <= 0 {
		limit = defaultListMax
	}
	if limit > maxListMax {
		limit = maxListMax
	}
	out := FindOutput{}
	if len(page) > limit {
		out.Truncated = true
		page = page[:limit]
		out.NextFrom = page[len(page)-1]
	}
	out.Paths = page

	var b strings.Builder
	for _, p := range page {
		b.WriteString(p)
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		b.WriteString("(no matches)\n")
	}
	if out.Truncated {
		fmt.Fprintf(&b, "... (%d shown; more remain — call again with from=%q)\n", len(page), out.NextFrom)
	}
	return textResult("%s", b.String()), out, nil
}
