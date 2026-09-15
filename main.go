// kbmcp is a small MCP server that exposes a single git-backed folder for
// browsing, full-text and filename search, targeted reads, and committed edits.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// loadInstructions reads the text handed to every client once at connect — it
// rides on both handshakes, initialize and server/discover. It lives in a file
// rather than in this source so it can be edited as prose, by whoever curates
// the vault, without a rebuild.
//
// A missing file is not an error: the server is fully usable without it, and a
// deployment that has nothing to explain should not have to carry an empty file.
// Any other read failure is worth a line, since it means the file is there and
// was meant to be used.
func loadInstructions(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("kbmcp: instructions %s: %v", path, err)
		}
		return ""
	}
	return string(b)
}

func main() {
	httpAddr := flag.String("http", "", "serve over HTTP on this address instead of stdio; host defaults to loopback (e.g. :8070, or 100.x.y.z:8070 to bind a Tailscale IP for a remote gateway)")
	token := flag.String("token", "", "bearer token for HTTP mode (default: $KBMCP_TOKEN, or KBMCP_TOKEN in the env file)")
	envPath := flag.String("env", ".env", "env file loaded at startup (KEY=VALUE lines); real env vars win; a missing file is ignored")
	instrPath := flag.String("instructions", "INSTRUCTIONS.md", "markdown file describing this server to connecting clients, sent once at connect; a missing file is ignored")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] [folder]\n\n"+
			"Serves [folder] (default: current directory) over MCP.\n"+
			"Default transport is stdio; use -http :PORT for Streamable HTTP.\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	// Load the env file before resolving env-derived config, so values in it are
	// visible to os.Getenv below (real environment variables take precedence).
	if err := loadEnvFile(*envPath); err != nil {
		log.Printf("kbmcp: env file %s: %v", *envPath, err)
	}
	if *token == "" {
		*token = os.Getenv("KBMCP_TOKEN")
	}

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

	server := newServer(loadInstructions(*instrPath))

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

// newServer builds the MCP server with every tool registered. Split out of
// main so a test can drive the real wiring — in particular the tool-list
// nudge, whose whole job happens at session setup.
func newServer(instructions string) *mcp.Server {
	// Armed after the tools are registered; see the nudge below. A session
	// cannot reach either trigger before then, since serving starts later, but
	// the nil check keeps that ordering from being load-bearing.
	var nudgeToolList func()

	server := mcp.NewServer(&mcp.Implementation{Name: "kbmcp", Version: "0.1.0"},
		&mcp.ServerOptions{Instructions: instructions})
	server.AddReceivingMiddleware(loggingMiddleware)

	// Fire the nudge at whichever point this client became able to receive
	// notifications. There are two, because go-sdk v1.7.0 added a second
	// handshake: a modern client calls server/discover and then subscriptions/
	// listen, never sending notifications/initialized at all, so
	// ServerOptions.InitializedHandler is dead for it. subscriptions/listen is
	// the better of the two triggers anyway — a modern client only sends it once
	// it has actually asked for tools-list-changed, so we notify exactly when
	// someone is listening. A client that subscribes to neither cannot be
	// reached at all; that is the protocol, not something to work around.
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			// subscriptions/listen does not return until the subscription ends,
			// so the nudge has to go out on the way in, not on the way back.
			// That is not too early: the send is deferred by the SDK's own 10ms
			// debounce, by which point next has registered the subscription.
			// nudgeToolList only schedules, it never blocks.
			if method == "subscriptions/listen" && nudgeToolList != nil {
				nudgeToolList()
				log.Printf("%ssubscribed; sent tools/list_changed", sessionTag(req))
			}
			res, err := next(ctx, method, req)
			if method == "notifications/initialized" && err == nil && nudgeToolList != nil {
				nudgeToolList()
				log.Printf("%sinitialized; sent tools/list_changed", sessionTag(req))
			}
			return res, err
		}
	})

	// Hoisted only so the nudge below has a tool to re-register; which tool that
	// is does not matter, and list_files is just the first one declared.
	listFilesTool := &mcp.Tool{
		Name:        "list_files",
		Description: "List files and folders within the served folder. Use 'path' to scope to a subfolder and 'recursive' to walk subfolders. Order with 'sort' ('path' default, or 'modified' for last-change time) and 'sort_reverse' (e.g. sort=modified + sort_reverse=true gives newest first). Paginated: at most 'max_results' entries (default 200); if 'truncated' is set, call again with 'from' set to the returned 'next_from' to get the next page.",
	}
	mcp.AddTool(server, listFilesTool, ListFiles)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search",
		Description: "Full-text search across the served folder (ripgrep). Pass exactly one of 'regex' (a regular expression, metacharacters special) or 'substring' (a literal string, metacharacters not special). Matching is smart-case unless 'case_sensitive' is set. Scope with 'path', restrict to filenames with 'glob' (e.g. '*.md'), cap with 'max_results'. Returns file paths with line numbers and the matching lines.",
	}, Search)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "find_files",
		Description: "Find files or directories by name (fd). Pass at most one of 'regex' (a regular expression matched against the filename) or 'glob' (a filename pattern such as '*.md'); passing neither lists everything under the scope. 'type' is 'file' (default) or 'dir'. Scope with 'path'. Returns sorted paths, paginated: at most 'max_results' (default 200); if 'truncated', call again with 'from' set to 'next_from'. Hidden files and .git are skipped.",
	}, FindFiles)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_frontmatter",
		Description: "Find notes by the values in their YAML frontmatter, and summarise those values with facets. 'filters' are conditions that must all hold; each operator names the type it reads ('date_lte' compares whole days, 'time_lte' instants, unprefixed lt/lte/gt/gte/eq are numeric, plus text_eq/text_contains/bool_eq/exists), and a field value that isn't of that type simply doesn't match. Dotted fields descend into maps and lists ('authentication.dkim', 'attachments.mime_type'). 'facets' summarise the whole match set: text_top (most common values + distinct count), range, date_range/time_range, date_bins/time_bins (histogram, UTC). Pass max_results=0 for facets only. Use read_frontmatter to see what fields a note actually has.",
	}, SearchFrontmatter)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_lines",
		Description: "Read a specific line range from a text file (1-based, inclusive). Output is line-numbered.",
	}, ReadLines)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_frontmatter",
		Description: "Read the raw YAML frontmatter block of one or more notes, unparsed. Pass 'paths' (get them from list_files, find_files, or search); the result has one entry per path, in the same order, each with the verbatim text between the leading '---' delimiters. Use this to see what fields and structure a note's frontmatter actually has without pulling the whole file. 'cap' bounds the bytes returned per file (default 2000); an over-long or unterminated block comes back with truncated=true — call again with a larger cap if you need the rest.",
	}, ReadFrontmatter)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_outline",
		Description: "List the '#', '##' and '###' headings of one or more notes, each with the line range its section spans (subsections included), to see how a long note is organised and then read just one section with read_lines. Pass 'paths'; the result has one entry per path, in the same order, with the note's line count. Headings inside frontmatter or fenced code blocks are not headings, and deeper headings belong to the section around them.",
	}, ReadOutline)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_file",
		Description: "Read whole text files from the served folder. Pass 'paths' (one or many) — the result has one entry per path, in the same order, so a set of search or list results can be pulled in a single call. 'cap' bounds the bytes per file; a call returns at most 1 MiB in total, and anything cut short is flagged with truncated. Use read_lines for a range within one file, or read_frontmatter for just the YAML header.",
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
		Name:        "delete_file",
		Description: "Delete a file, then commit the removal. Only a file whose current content is committed can be deleted, so every deletion can be undone from history (file_at at the commit before). Folders left empty are removed; a symlink is removed itself, not the file it points to. A commit 'message' is required. Pass dry_run to check the deletion and preview the diff without deleting or committing.",
	}, DeleteFile)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "move_file",
		Description: "Move or rename a file, then commit it. Every [[wiki link]] in the vault that resolved before the move is rewritten, where needed, to resolve to the same note after it — links to the moved note, the moved note's own relative links, and links elsewhere that a new name in a folder would otherwise capture or make ambiguous. Already-broken links are left alone. The move and the rewrites are one commit. Refused if the destination exists, or if the file or any note needing a rewrite has uncommitted changes. A commit 'message' is required. Pass dry_run to see the rewrites and diffs without writing or committing.",
	}, MoveFile)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "batch_edits",
		Description: "Apply an ordered mix of write (new/overwritten files), edit (string replacements), delete (file removal, same rules as delete_file), and move (same rules as move_file, including link rewrites) operations across one or more files and commit them together as a single commit. All ops are validated first; if any is invalid, nothing is written. A commit 'message' is required. Pass dry_run to preview all diffs without writing or committing.",
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

	// A restart leaves a connected client holding the tool list it fetched
	// before. It cannot keep its session — an unknown Mcp-Session-Id gets a 404,
	// which forces a fresh initialize — but a client that caches the tool list
	// may carry it across that reconnect and keep calling a parameter that has
	// since been renamed. So every time a session finishes initializing, tell it
	// the list is stale.
	//
	// What goes out is one content-free line,
	// {"jsonrpc":"2.0","method":"notifications/tools/list_changed"}. It carries
	// no tool data; the client has to call tools/list itself, and the current
	// definitions travel in the response to that. MCP has no message that pushes
	// tool definitions, so invalidation is the only lever a server has — and it
	// only reaches a client holding the GET stream open.
	//
	// Re-adding an identical tool is that lever: Server.AddTool reports "changed"
	// unconditionally, and notifySessions (which would let us target one session)
	// is unexported as of go-sdk v1.7.0. So this reaches every session with an
	// open stream rather than just the new one — wasteful in steady state, and
	// exactly right after a restart, when they are all new.
	//
	// Because no session survives a restart, once-per-session is already
	// once-per-restart-per-client: there is no "did we notify yet" state to keep.
	nudgeToolList = func() { mcp.AddTool(server, listFilesTool, ListFiles) }

	return server
}
