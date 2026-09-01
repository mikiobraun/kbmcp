# kbmcp

A small [MCP](https://modelcontextprotocol.io) server (stdio) written in Go that
exposes a single folder on your filesystem for browsing, substring search,
targeted reads, and **git-backed edits**: every write is committed, and agents
can explore the resulting history. Built on the official
[`go-sdk`](https://github.com/modelcontextprotocol/go-sdk).

The served folder **must be a git repository** — the server refuses to start
otherwise. Configure a commit identity (`git config user.name` / `user.email`)
in it so commits succeed.

## Tools

| Tool | Arguments | Description |
|------|-----------|-------------|
| `list_files` | `path` (optional), `recursive` (optional) | List entries under the served folder. |
| `search` | `query`, `path` (optional scope), `max_results` (optional, default 100) | Case-insensitive substring search across text files. Returns `file:line: text`. |
| `read_lines` | `path`, `start` (default 1), `end` (default EOF) | Read a 1-based inclusive line range, line-numbered. |
| `read_file` | `path` | Read a whole text file (truncated at 1 MiB). |
| `write_file` | `path`, `content`, `message`, `author_name`/`author_email` (optional), `dry_run` (optional) | Create or overwrite a file, then commit it. Parent folders are created. Returns a diff. |
| `edit_file` | `path`, `old_string`, `new_string`, `message`, `author_name`/`author_email` (optional), `replace_all` (optional), `dry_run` (optional) | Replace exact text, then commit. `old_string` must be unique unless `replace_all`. Returns a diff. |
| `batch_edits` | `message`, `ops` (each `{op: "write"\|"edit", …}`), `author_name`/`author_email` (optional), `dry_run` (optional) | Apply an ordered mix of writes and edits atomically and commit them as a **single** commit. |
| `history` | `path` (optional), `max` (optional, default 20), `since` (optional ref) | Compact commit log: short hash, relative time, author, subject. `--follow`s renames for a single file. |
| `diff` | `from` (default `HEAD~1`), `to` (default `HEAD`), `path` (optional), `stat` (optional) | Unified diff between two commits; `stat: true` gives a per-file insertion/deletion summary. |
| `file_at` | `path`, `ref` | Read a file's contents as of a given commit ref. |
| `backlinks` | `path` | Notes that link to this note via `[[wiki links]]`, with the source line of each. |
| `outgoing_links` | `path` | The `[[wiki links]]` in this note, showing which resolve and which are broken. |
| `orphans` | — | Notes that nothing else links to (no backlinks). |

All paths are relative to the served folder. Requests that escape the folder
(`../`), absolute paths, and binary files are refused.

Wiki-links use Obsidian-style resolution: `[[note-name]]` matches the file named
`note-name.md` anywhere in the vault (by basename, case-insensitive); an
`|alias` or `#heading` suffix is ignored, and `[[...]]` inside code spans or
fenced code blocks is not treated as a link.

The write tools return a unified-style diff of the change, and accept
`dry_run: true` to preview that diff without writing or committing anything. For
very large files the diff is skipped (a summary line is returned instead) to
avoid the O(N×M) cost of the line-diff — similar to how GitHub hides diffs for
huge files. Note that the approval/confirmation prompt for a write is shown by
the **client** (e.g. Claude Desktop's "allow tool" dialog), not by this server —
MCP has no server-rendered diff-approval UI.

## Git-backed writes & history

Every successful `write_file`, `edit_file`, and `batch_edits` commits its change,
so the served folder's git history is a complete, inspectable log of edits — the
point being that when several agents work the same vault, "what changed, by whom"
is answerable. A commit `message` is **required**; `batch_edits` groups an
ordered mix of writes (new/overwritten files) and edits (string replacements)
into one commit, validating every op first so a single bad op writes nothing.

Pass `author_name` / `author_email` to attribute a commit to the agent making
it (they override the committer identity for that commit only); omit them to fall
back to the repo's configured identity.

The `history`, `diff`, and `file_at` tools read that history back. Use `history`
with `since` (a ref you already know) to see just what's changed since you last
looked, `diff` to inspect a change (`stat: true` for a summary first, to keep
large diffs from flooding the context), and `file_at` to fetch an old version of
a file. Diffs and file reads are size-capped and truncated; caller-supplied refs
are validated (no option injection) and paths stay confined to the folder.

## REST interface

In HTTP mode the server also exposes a small REST surface under `/files`,
alongside the MCP endpoint and behind the same auth:

| Method | Path | Body | Description |
|--------|------|------|-------------|
| `GET` | `/files/<path>` | — | A file's raw content (markdown as `text/markdown`), or a JSON listing for a directory. |
| `PUT` | `/files/<path>` | raw file content | Create or overwrite the file, then commit it. Returns `201`/`200`, an `ETag`, and a small JSON body. |

`PUT` carries commit metadata in the query string, so the body stays pure
content: `?message=…` (**required**) plus optional `?author_name=…&author_email=…`.
It commits through the exact same path as `write_file`.

Conditional requests give agents optimistic locking against each other, using the
`ETag` from a prior `GET`:

- `If-Match: <etag>` — overwrite only if the file is unchanged, else `412`.
- `If-None-Match: *` — create only; `412` if the file already exists.

```sh
curl -X PUT 'http://host:8080/files/notes/idea.md?message=Add+idea&author_name=alice&author_email=alice@x' \
     -H 'Content-Type: text/markdown' --data-binary @idea.md
```

The REST write is read/create/overwrite only — no `DELETE`, and the `edit_file`
find/replace stays a tool-only affordance.

## Build

```sh
go build -o kbmcp .
```

## Run

### stdio (default — local, launched by the client)

```sh
./kbmcp /path/to/folder      # defaults to the current directory
```

The folder must be a git repository (`git init` it first); the server exits with
an error otherwise. It speaks JSON-RPC over stdin/stdout — meant to be launched
by an MCP client, not run interactively.

### HTTP (centralized — reachable over the network)

Serves the same tools over Streamable HTTP. A bearer token is **required**; set
it via `$KBMCP_TOKEN` (preferred — keeps it out of the process list) or `-token`.

```sh
KBMCP_TOKEN=your-secret ./kbmcp -http :8080 /path/to/folder
```

Every request must carry `Authorization: Bearer your-secret`; anything else gets
`401`.

**Network safety:** traffic is plain HTTP, so the token and file contents are
unencrypted on the wire. Only expose this on a trusted network — a home LAN or a
[Tailscale](https://tailscale.com) tailnet. For access across the open internet,
put it behind a TLS-terminating reverse proxy (Caddy/nginx) rather than exposing
the port directly.

## Logs

The server logs every request to stderr: each tool call with its name, a
(truncated) summary of its arguments, whether it succeeded or `FAILED` (with the
error message), and how long it took. Other methods (`initialize`, `tools/list`)
are logged briefly.

```
tool search {"query":"alpha"} -> ok (2ms)
tool edit_file {"path":"notes.md","old_string":"foo",...} -> FAILED: old_string not found in notes.md (1ms)
```

Watch them live by running the server in a terminal, or — once it runs under
systemd — with `journalctl -fu kbmcp`.

## Wire it into a client

Claude Code:

```sh
claude mcp add kb -- /absolute/path/to/kbmcp /path/to/folder
```

Or, for Claude Desktop / any client using a config file:

```json
{
  "mcpServers": {
    "kb": {
      "command": "/absolute/path/to/kbmcp",
      "args": ["/path/to/folder"]
    }
  }
}
```

For a remote (HTTP) server, point clients at the URL with the bearer header:

```sh
claude mcp add --transport http kb http://your-host:8080/ \
  --header "Authorization: Bearer your-secret"
```

```json
{
  "mcpServers": {
    "kb": {
      "url": "http://your-host:8080/",
      "headers": { "Authorization": "Bearer your-secret" }
    }
  }
}
```

## Notes / not-yet-done

- HTTP auth is a single shared bearer token; no per-user identity or OAuth.
- No built-in TLS — terminate TLS at a reverse proxy for internet exposure.
- Search is plain case-insensitive substring (no regex), and walks the whole
  tree on each call with no ignore-list.
- Single served folder per process.
