package main

// find_files: name-based discovery, backed by fd. A separate tool from search
// on purpose — its result shape is a list of paths, not content matches. Like
// ripgrep, fd skips hidden files and .git by default, and does not follow
// symlinks unless asked, so the scope stays inside the vault.
//
// Ignore files are not consulted (--no-ignore), matching search: whether a path
// is gitignored, or listed in an .ignore, is not the decision about whether it
// is in the vault. The file listing shows it either way.

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

// As in SearchInput, the matching mode is the name of the field carrying the
// pattern rather than a flag beside it, so a call cannot silently be read in
// the mode the caller didn't mean. Unlike search, neither is required: with no
// pattern at all, fd lists everything under the scope, which is a useful call.
type FindInput struct {
	Regex      string `json:"regex,omitempty" jsonschema:"a regular expression matched against the filename; metacharacters are special. Pass at most one of regex or glob."`
	Glob       string `json:"glob,omitempty" jsonschema:"a filename glob such as '*.md'; metacharacters carry their glob meaning, not their regex one. Pass at most one of regex or glob."`
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
	out, err := findCore(ctx, in)
	if err != nil {
		return nil, FindOutput{}, err
	}

	var b strings.Builder
	for _, p := range out.Paths {
		b.WriteString(p)
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		b.WriteString("(no matches)\n")
	}
	if out.Truncated {
		fmt.Fprintf(&b, "... (%d shown; more remain — call again with from=%q)\n", len(out.Paths), out.NextFrom)
	}
	return textResult("%s", b.String()), out, nil
}

// findCore does the finding. Like searchCore it mentions no MCP types, so the
// REST handler calls it directly rather than fabricating a tool request.
func findCore(ctx context.Context, in FindInput) (FindOutput, error) {
	all, err := fdPaths(ctx, in)
	if err != nil {
		return FindOutput{}, err
	}

	// Take the page strictly after the cursor.
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

	return out, nil
}

// fdPaths runs fd and returns every match, sorted and uncapped. findCore pages
// and caps on top of it; the wiki-link note index needs all of them, and paging
// there would re-run fd once per page.
func fdPaths(ctx context.Context, in FindInput) ([]string, error) {
	hasRe := strings.TrimSpace(in.Regex) != ""
	hasGlob := strings.TrimSpace(in.Glob) != ""
	if hasRe && hasGlob {
		return nil, fmt.Errorf("pass at most one of 'regex' or 'glob', not both: 'regex' is a regular expression matched against the filename (metacharacters like [ ] . * ? are special); 'glob' is a shell-style filename pattern such as '*.md'. Passing neither matches everything under the scope")
	}
	scope, err := resolve(in.Path)
	if err != nil {
		return nil, err
	}
	fd, err := fdBinary()
	if err != nil {
		return nil, err
	}

	typ := "f"
	switch in.Type {
	case "", "file", "f":
		typ = "f"
	case "dir", "directory", "d":
		typ = "d"
	default:
		return nil, fmt.Errorf("type must be 'file' or 'dir', got %q", in.Type)
	}

	// Run from root with a relative --search-path so paths come back relative to
	// root. The flag form (not a positional) keeps the scope from being mistaken
	// for the pattern.
	// --no-ignore matches search: no ignore file decides what is in the vault.
	// It does not imply --hidden, so dotfiles and .git stay out on fd's own rule.
	args := []string{"--color=never", "--no-ignore", "--type", typ, "--search-path", relPath(scope)}
	// Use the raw value, not the trimmed one — the trims above only tested
	// presence, and surrounding space can be part of a filename pattern.
	pattern := in.Regex
	if hasGlob {
		pattern = in.Glob
		args = append(args, "--glob")
	}
	if pattern != "" {
		args = append(args, "--", pattern)
	}

	cmd := exec.CommandContext(ctx, fd, args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
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
		return nil, fmt.Errorf("fd: %s", msg)
	}

	sort.Strings(all) // stable order, and what the cursor paging assumes
	return all, nil
}
