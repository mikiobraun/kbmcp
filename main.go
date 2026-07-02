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
	log.Printf("kbmcp: serving %s", root)

	server := mcp.NewServer(&mcp.Implementation{Name: "kbmcp", Version: "0.1.0"}, nil)
	server.AddReceivingMiddleware(loggingMiddleware)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_files",
		Description: "List files and folders within the served folder. Use 'path' to scope to a subfolder and 'recursive' to walk subfolders.",
	}, ListFiles)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search",
		Description: "Case-insensitive substring search across text files in the served folder. Returns file paths with line numbers and the matching lines.",
	}, Search)

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
		Description: "Create or overwrite a text file with the given content. Parent folders are created as needed. Pass dry_run to preview the diff without writing.",
	}, WriteFile)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "edit_file",
		Description: "Replace an exact string in a text file. old_string must occur exactly once unless replace_all is set. Pass dry_run to preview the diff without writing.",
	}, EditFile)

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
