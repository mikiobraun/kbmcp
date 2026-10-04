package main

// search_frontmatter: the MCP face of fmq (cmd/fmq, contract in its SPEC.md).
//
// kbmcp runs fmq as a separate binary, the way search runs rg, so that any
// implementation of the contract can take its place — one backed by an index,
// say — without a change here. What stays here is what only kbmcp can do:
// confining the scope to the vault, and relaying fmq's errors to the agent
// verbatim, since they carry the fix (e.g. "use int_lte").

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mikiobraun/kbmcp/fmq"
)

// fmqCommand is the fmq binary, looked up on PATH. Tests point it at a fresh
// build.
var fmqCommand = "fmq"

type SearchFrontmatterInput struct {
	Filters []fmq.Filter `json:"filters,omitempty" jsonschema:"conditions on frontmatter fields, all of which must hold (AND). Empty matches every note that has frontmatter."`
	Facets  []fmq.Facet  `json:"facets,omitempty" jsonschema:"summaries to compute over the whole match set (not just the returned page)"`
	Fields  []string     `json:"fields,omitempty" jsonschema:"frontmatter fields to return with each match, so you need not read the files; each comes back as the list of values a filter on it would see ([] when missing)"`
	Path    string       `json:"path,omitempty" jsonschema:"folder to scope the scan to, relative to root; empty means the whole root"`
	Sort    string       `json:"sort,omitempty" jsonschema:"sort order: 'path' (default) or 'modified' (file's last-change time, which for imported notes is import time, not the note's own date)"`
	Reverse bool         `json:"sort_reverse,omitempty" jsonschema:"reverse the sort order; e.g. sort=modified + sort_reverse=true lists newest first"`
	// A pointer so that "absent" and "zero" are distinguishable: 0 is a useful
	// request (facets only, no document list) and must not read as "unset".
	MaxResults *int `json:"max_results,omitempty" jsonschema:"maximum matches to return (default 200, capped at 1000). Pass 0 for facets only, with no document list."`
}

type SearchFrontmatterOutput = fmq.Result

func SearchFrontmatter(ctx context.Context, req *mcp.CallToolRequest, in SearchFrontmatterInput) (*mcp.CallToolResult, SearchFrontmatterOutput, error) {
	scope, err := resolve(in.Path)
	if err != nil {
		return nil, SearchFrontmatterOutput{}, err
	}
	// fmq returns every match unless told otherwise; the budget is an agent's
	// context window, so it is kbmcp's to set. Negative passes through for
	// fmq to refuse.
	limit := defaultListMax
	if in.MaxResults != nil {
		limit = min(*in.MaxResults, maxListMax)
	}
	out, err := runFmq(ctx, fmq.Query{
		Filters:    in.Filters,
		Facets:     in.Facets,
		Fields:     in.Fields,
		Sort:       in.Sort,
		Reverse:    in.Reverse,
		MaxResults: &limit,
	}, relPath(scope))
	if err != nil {
		return nil, SearchFrontmatterOutput{}, err
	}
	return textResult("%s", fmq.Render(out)), out, nil
}

// runFmq sends the query as JSON on stdin and reads the JSON result. Like rg,
// fmq runs from root with a relative scope, so its paths come back relative to
// root; "--" keeps a folder named like a flag from being read as one.
func runFmq(ctx context.Context, q fmq.Query, scope string) (fmq.Result, error) {
	bin, err := exec.LookPath(fmqCommand)
	if err != nil {
		return fmq.Result{}, fmt.Errorf("search_frontmatter requires fmq, which is not installed")
	}
	query, err := json.Marshal(q)
	if err != nil {
		return fmq.Result{}, err
	}
	cmd := exec.CommandContext(ctx, bin, "-query-json", "-", "-o", "json", "--", scope)
	cmd.Dir = root
	cmd.Stdin = bytes.NewReader(query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return fmq.Result{}, errors.New(strings.TrimPrefix(msg, "fmq: "))
			}
		}
		return fmq.Result{}, fmt.Errorf("fmq: %w", err)
	}
	var out fmq.Result
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return fmq.Result{}, fmt.Errorf("fmq: unreadable result: %w", err)
	}
	return out, nil
}
