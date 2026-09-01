package main

// The search subsystem. Today it is content search backed by ripgrep; it is the
// place future modes land — frontmatter-field search next, and eventually
// LanceDB-backed semantic search.
//
// ripgrep is used deliberately: it is fast, its regex engine is linear-time (no
// catastrophic backtracking on a hostile pattern), and by default it skips
// hidden files, .git, and gitignored paths — matching the vault's own dotfile
// filtering for free.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type SearchInput struct {
	Query      string `json:"query" jsonschema:"the pattern to search for; a regular expression unless fixed_strings is set"`
	Path       string `json:"path,omitempty" jsonschema:"folder to scope the search to, relative to root; empty means the whole root"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"maximum number of matches to return (default 100, capped at 1000)"`
	Fixed      bool   `json:"fixed_strings,omitempty" jsonschema:"treat query as a literal string instead of a regular expression"`
	Sensitive  bool   `json:"case_sensitive,omitempty" jsonschema:"match case-sensitively; default is smart case (case-insensitive unless the query contains an uppercase letter)"`
	Glob       string `json:"glob,omitempty" jsonschema:"restrict the search to files whose name matches this glob (e.g. '*.md'); prefix with '!' to exclude. Empty searches all files."`
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

const (
	defaultSearchMax = 100
	maxSearchMax     = 1000
)

// rgEvent is the subset of ripgrep's --json event stream we consume.
type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path       struct{ Text string } `json:"path"`
		Lines      struct{ Text string } `json:"lines"`
		LineNumber int                   `json:"line_number"`
	} `json:"data"`
}

// Search runs ripgrep over the (path-confined) scope and returns matches with
// file, line number, and the matching line. Results are capped; Truncated says
// whether more matches existed beyond the cap.
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
		limit = defaultSearchMax
	}
	if limit > maxSearchMax {
		limit = maxSearchMax
	}

	rg, err := exec.LookPath("rg")
	if err != nil {
		return nil, SearchOutput{}, fmt.Errorf("search requires ripgrep (rg), which is not installed")
	}

	args := []string{"--json", "--line-number"}
	if in.Fixed {
		args = append(args, "--fixed-strings")
	}
	if in.Sensitive {
		args = append(args, "--case-sensitive")
	} else {
		args = append(args, "--smart-case")
	}
	if g := strings.TrimSpace(in.Glob); g != "" {
		args = append(args, "--glob", g)
	}
	// Confine ripgrep to the scope by running from root and passing the scope as
	// a relative path, so match paths come back relative to root. "." is root.
	args = append(args, "--", in.Query, relPath(scope))

	// Cancel ripgrep as soon as we have enough matches instead of draining it.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, rg, args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, SearchOutput{}, err
	}
	if err := cmd.Start(); err != nil {
		return nil, SearchOutput{}, err
	}

	var out SearchOutput
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // match lines can be long
	for sc.Scan() {
		var ev rgEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Type != "match" {
			continue
		}
		out.Matches = append(out.Matches, Match{
			// When scoped to root we pass "." to rg, which prefixes paths with
			// "./"; strip it so paths read relative to root like everywhere else.
			Path: strings.TrimPrefix(ev.Data.Path.Text, "./"),
			Line: ev.Data.LineNumber,
			Text: strings.TrimRight(ev.Data.Lines.Text, "\r\n"),
		})
		if len(out.Matches) >= limit {
			out.Truncated = true
			cancel()
			break
		}
	}
	werr := cmd.Wait()
	// Exit 1 = no matches (fine); >=2 = a real error (e.g. a bad regex). Ignore
	// errors once we've cancelled after hitting the cap.
	if !out.Truncated && werr != nil {
		if ee, ok := werr.(*exec.ExitError); ok && ee.ExitCode() >= 2 {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = werr.Error()
			}
			return nil, SearchOutput{}, fmt.Errorf("ripgrep: %s", msg)
		}
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
