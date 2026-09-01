# kbmcp

A small [MCP](https://modelcontextprotocol.io) server (stdio) written in Go that
exposes a single folder on your filesystem for browsing, full-text and filename
search, targeted reads, and **git-backed edits**: every write is committed, and
agents can explore the resulting history. Built on the official
[`go-sdk`](https://github.com/modelcontextprotocol/go-sdk).

The served folder **must be a git repository** — the server refuses to start
otherwise. Configure a commit identity (`git config user.name` / `user.email`)
in it so commits succeed.

## Tools

| Tool | Arguments | Description |
|------|-----------|-------------|
| `list_files` | `path`, `recursive`, `sort` (`path`\|`modified`), `sort_reverse`, `max_results` (default 200), `from` — all optional | List entries under the served folder, sorted and paginated. `sort: modified` + `sort_reverse: true` gives newest first. |
| `search` | `query`, `path` (scope), `glob`, `fixed_strings`, `case_sensitive`, `max_results` (default 100) | Full-text content search via ripgrep. `query` is a regex unless `fixed_strings`; smart-case unless `case_sensitive`; `glob` restricts by filename. Returns `file:line: text`. |
| `find_files` | `pattern`, `glob`, `type` (`file`\|`dir`), `path` (scope), `max_results` (default 200), `from` — all optional | Find files/directories by name via fd. `pattern` is a regex unless `glob`. Returns a sorted, paginated path list. |
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
(`../`), absolute paths, and binary files are refused. Hidden entries — dotfiles
and dot-directories such as `.git` — are excluded from every listing, search,
and the link graph, and pagination cursors (`from`/`next_from`) let an agent page
a large vault without pulling the whole tree at once.

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

## Search & discovery

Two complementary tools, deliberately separate because their result shapes
differ — content matches vs. a list of paths:

- `search` — **content** search backed by
  [ripgrep](https://github.com/BurntSushi/ripgrep): a regular expression (or a
  `fixed_strings` literal), smart- or exact-case, optionally narrowed by `path`
  (a subtree) and `glob` (filenames, e.g. `*.md`, or `!*.log` to exclude).
  Returns line matches, capped by `max_results` and streamed so a broad query
  stops early rather than draining ripgrep.
- `find_files` — **name** discovery backed by
  [fd](https://github.com/sharkdp/fd): a regex or `glob` name pattern, `type`
  file/dir, `path` scope. Returns a sorted, cursor-paginated list of paths.

Both require their binary on `PATH` (`rg`, `fd`), confine the scope inside the
served folder, do not follow symlinks, and skip hidden files, `.git`, and
gitignored paths by default.

Frontmatter-field search and LanceDB-backed semantic search are planned to land
as their own tools, on the same principle — one tool per result shape.

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

Serves the same MCP tools and the REST surface over Streamable HTTP. There are
two modes, chosen by whether a token is set.

**Standalone, with a bearer token** — binds all interfaces (`0.0.0.0`) and
requires `Authorization: Bearer <token>` on every request (constant-time
compared; anything else gets `401`). Set the token via `$KBMCP_TOKEN`
(preferred — keeps it out of the process list) or `-token`:

```sh
KBMCP_TOKEN=your-secret ./kbmcp -http :8080 /path/to/folder
```

**Behind an auth gateway (no token)** — with no token set, the server binds
`127.0.0.1` only and does no auth of its own, trusting a reverse proxy in front
of it (e.g. Caddy `forward_auth`) to authenticate the caller and inject their
identity as `X-Volume-User` / `X-Volume-Scopes` headers. Because only localhost
can connect, those headers are trustworthy; the server logs them but does not
itself enforce scopes. This is how it runs behind an OAuth gateway:

```sh
./kbmcp -http :8070 /path/to/folder      # loopback only; the gateway does auth
```

**Network safety:** traffic is plain HTTP, so in token mode the bearer token and
file contents are unencrypted on the wire — only expose it on a trusted network,
a home LAN or a [Tailscale](https://tailscale.com) tailnet. For internet
exposure, prefer the gateway mode: a TLS-terminating reverse proxy (Caddy/nginx)
handles TLS and auth, and the server itself never leaves loopback.

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

- HTTP auth is either a single shared bearer token, or delegated to a fronting
  gateway (which can add per-user identity / OAuth); the server logs the injected
  identity but does not enforce scopes itself.
- No built-in TLS — terminate TLS at a reverse proxy for internet exposure.
- `search` and `find_files` shell out to `rg` and `fd` — both must be installed
  on the host.
- Single served folder per process.
