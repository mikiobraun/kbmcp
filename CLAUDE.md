# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`kbmcp` is a single-package Go MCP server (module `kbmcp`, all files `package main`)
that exposes **one git-backed folder** — a markdown vault — over MCP (stdio or
Streamable HTTP) plus a small REST surface. Every write is a git commit; history
is a first-class, queryable part of the API. Built on
`github.com/modelcontextprotocol/go-sdk`.

## Commands

```sh
go build -o kbmcp .          # build
go test ./...                # all tests
go test -run TestSearchSmartCase -v ./...   # single test
go vet ./...
./kbmcp /path/to/vault                       # run: stdio (folder must be a git repo)
KBMCP_TOKEN=secret ./kbmcp -http :8070 /path/to/vault   # run: HTTP
```

Tests that need `rg`, `fd`, or `git` skip themselves when the binary is missing
(see `requireRg` in `search_test.go`). `go test` needs `git` for the write/history
tests.

## Architecture

**Confinement is the load-bearing invariant.** `safe.go` holds a package-level
`root` (absolute, symlink-resolved) set once by `setRoot`. Every caller-supplied
path goes through `resolve()`, which rejects absolute paths, `../` escapes, and
**any hidden path segment** (so `.git` is unreachable — a write can't corrupt the
vault's own repo). New tools and REST handlers must route through `resolve()`;
render paths back for output with `relPath()`. Listings, search, and the link
graph independently skip dotfiles via `isHidden`.

**Tool handlers** are `func(ctx, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error)`,
registered with `mcp.AddTool` in `main.go` (the tool *description* an agent sees
lives there, next to the registration; per-field docs are `jsonschema:` tags on the
input struct). Each handler returns **both** a human-readable text result
(`textResult`) and a typed struct — keep both in sync. Adding a tool = new file
+ registration in `main.go`.

**Git-backed writes.** `git.go` is the only place that runs git: `runGit` for
read-only commands, `gitCommit` for pathspec-limited commits guarded by `gitMu`.
`writeAndCommit` (`tools.go`) is the shared write core for `write_file` and REST
`PUT`, so behavior stays identical across surfaces. `batch_edits` validates every
op against evolving in-memory content (`batchState`, `planBatch`) *before*
touching disk, so a bad op writes nothing; `move_file` and REST `POST /move` are
a one-op batch (`batchState.move` in `move.go`). Refs from callers are
validated by `validRef` (no leading `-`, no `..` ranges) before reaching git.

**Wiki links** resolve against a `vaultView` (`wikilinks.go`) — every path mapped
to the file it resolves to, from one fd run — never against disk directly. That
is what lets a move resolve every link against the vault before and after and
rewrite exactly those whose target changed. Anything that resolves links must
take a `vaultView`, or a link will mean different things on different surfaces.

**External binaries.** `search` shells out to ripgrep (`--json` stream, cancelled
early once `max_results` is hit rather than drained) and `find_files` to fd
(`fd`/`fdfind`). Both are run with `cmd.Dir = root` and a *relative* scope so
paths come back relative to root, and both inherit rg/fd's default skipping of
hidden files, `.git`, and gitignored paths. `search.go`'s header comment marks it
as the home for future search modes (frontmatter-field search, then semantic).

**Pagination** is stateless throughout: `from` is an exclusive lower bound on the
sort key and `next_from` is the cursor to pass back (`entrySortKey` in `tools.go`
builds the composite key for `sort=modified`). Everything is capped —
`maxFileBytes` (1 MiB) on reads, `maxListMax`/`maxSearchMax` on result counts,
diffs skipped for very large files — because the consumer is an agent context
window.

**Transports.** `main.go` builds one `mcp.Server` and either runs it over stdio or
hands it to `serveHTTP` (`http.go`), which mounts MCP at `/` and REST
(`GET/PUT /files/`, `GET /history`) on the same mux behind the same bearer-token
middleware. In HTTP mode kbmcp is meant to sit behind an auth gateway (Caddy
`forward_auth` → volume-auth); the shared token proves the caller *is* the
gateway, which is what makes the injected `X-Volume-User` / `X-Volume-Scopes`
headers trustworthy (logged, not enforced). `rest.go` supports `If-Match` /
`If-None-Match` ETag optimistic locking.

**Logging** is a receiving middleware (`log.go`) that logs every request with a
summarized argument line and timing; it never logs the token, and truncates
payloads.

**Config** comes from flags, then `$KBMCP_TOKEN`, with `.env` (`env.go`) as a
fallback that never overrides the real environment.

## Conventions

- Tests are table-free, one behavior per `TestXxx`, using helpers `newRepo`
  (temp git repo + `setRoot`) and `searchVault` (temp files + `setRoot`). Because
  `root` is package-level global state, **tests must not call `t.Parallel()`**.
- Comments explain *why* (a security or agent-ergonomics rationale), not what.
  The existing code is densely commented in that style; match it.
- README.md is user-facing documentation and is kept in sync with behavior
  changes — update it in the same change, not later.
- BACKLOG.md holds deliberate not-yet-done work, with the reasoning that led
  there. Add an entry when a decision would otherwise have to be re-derived;
  delete it when the work lands.
- `dev.yaml` describes this service to the auth gateway; it is not read by this
  binary and is gitignored — it names a deployment's own hostnames, which do not
  belong in a public repo.
