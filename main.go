// kbmcp is a small MCP server (stdio) that exposes a single folder for
// browsing, substring search, and targeted reads.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	httpAddr := flag.String("http", "", "serve over HTTP on this address (e.g. :8080) instead of stdio")
	token := flag.String("token", os.Getenv("KBMCP_TOKEN"), "bearer token required in HTTP mode (defaults to $KBMCP_TOKEN)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] [folder]\n\n"+
			"Serves [folder] (default: current directory) over MCP.\n"+
			"Default transport is stdio; use -http :PORT for Streamable HTTP.\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	dir := "."
	if flag.NArg() > 0 {
		dir = flag.Arg(0)
	}
	if err := setRoot(dir); err != nil {
		log.Fatalf("kbmcp: %v", err)
	}
	if !isGitRepo() {
		log.Fatalf("kbmcp: %s is not a git repository; run 'git init' there and set user.name/user.email (writes are committed to its history)", root)
	}
	log.Printf("kbmcp: serving %s", root)

	server := mcp.NewServer(&mcp.Implementation{Name: "kbmcp", Version: "0.1.0"}, nil)
	server.AddReceivingMiddleware(loggingMiddleware)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_files",
		Description: "List files and folders within the served folder. Use 'path' to scope to a subfolder and 'recursive' to walk subfolders. Order with 'sort' ('path' default, or 'modified' for last-change time) and 'sort_reverse' (e.g. sort=modified + sort_reverse=true gives newest first). Paginated: at most 'max_results' entries (default 200); if 'truncated' is set, call again with 'from' set to the returned 'next_from' to get the next page.",
	}, ListFiles)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search",
		Description: "Full-text search across the served folder (ripgrep). 'query' is a regular expression unless 'fixed_strings' is set; matching is smart-case unless 'case_sensitive' is set. Scope with 'path', restrict to filenames with 'glob' (e.g. '*.md'), cap with 'max_results'. Returns file paths with line numbers and the matching lines.",
	}, Search)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "find_files",
		Description: "Find files or directories by name (fd). 'pattern' is a regular expression unless 'glob' is set (e.g. '*.md'). 'type' is 'file' (default) or 'dir'. Scope with 'path'. Returns sorted paths, paginated: at most 'max_results' (default 200); if 'truncated', call again with 'from' set to 'next_from'. Hidden files and .git are skipped.",
	}, FindFiles)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_lines",
		Description: "Read a specific line range from a text file (1-based, inclusive). Output is line-numbered.",
	}, ReadLines)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_file",
		Description: "Read a whole text file from the served folder (truncated if very large).",
	}, ReadFile)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "write_file",
		Description: "Create or overwrite a text file with the given content, then commit it. A commit 'message' is required. Parent folders are created as needed. Pass dry_run to preview the diff without writing or committing.",
	}, WriteFile)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "edit_file",
		Description: "Replace an exact string in a text file, then commit it. old_string must occur exactly once unless replace_all is set. A commit 'message' is required. Pass dry_run to preview the diff without writing or committing.",
	}, EditFile)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "batch_edits",
		Description: "Apply an ordered mix of write (new/overwritten files) and edit (string replacements) operations across one or more files and commit them together as a single commit. All ops are validated first; if any is invalid, nothing is written. A commit 'message' is required. Pass dry_run to preview all diffs without writing or committing.",
	}, BatchEdits)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "history",
		Description: "Compact commit history: short hash, relative time, author, and subject. Scope to a file or folder with 'path' (renames are followed for a single file), cap with 'max', or pass 'since' (a ref) to see only what changed after a point you already know.",
	}, History)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "diff",
		Description: "Show a unified diff between two commits (defaults from=HEAD~1 to=HEAD). Scope with 'path', or pass stat=true for a compact per-file summary of insertions/deletions. Renames are detected.",
	}, Diff)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "file_at",
		Description: "Read the contents of a file as it was at a given commit ref (e.g. a hash from history, or HEAD~1).",
	}, FileAt)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "backlinks",
		Description: "List notes that link to the given note via [[wiki links]], with the source line of each link.",
	}, Backlinks)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "outgoing_links",
		Description: "List the [[wiki links]] in the given note, showing which resolve to a note and which are broken.",
	}, OutgoingLinks)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "orphans",
		Description: "List notes that nothing else links to (no backlinks).",
	}, Orphans)

	if *httpAddr != "" {
		if err := serveHTTP(server, *httpAddr, *token); err != nil {
			log.Fatalf("kbmcp: %v", err)
		}
		return
	}

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("kbmcp: %v", err)
	}
}
