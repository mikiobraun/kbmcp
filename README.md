# kbmcp

A small [MCP](https://modelcontextprotocol.io) server written in Go (stdio or HTTP) that
exposes a single folder on your filesystem for browsing, full-text, filename and
frontmatter search, targeted reads, and **git-backed edits**: every write is committed, and
agents can explore the resulting history. Built on the official
[`go-sdk`](https://github.com/modelcontextprotocol/go-sdk).

The served folder **must be a git repository** — the server refuses to start
otherwise. Configure a commit identity (`git config user.name` / `user.email`)
in it so commits succeed.

## Tools

| Tool | Arguments | Description |
|------|-----------|-------------|
| `list_files` | `path`, `recursive`, `sort` (`path`\|`modified`), `sort_reverse`, `max_results` (default 200), `from` — all optional | List entries under the served folder, sorted and paginated. `sort: modified` + `sort_reverse: true` gives newest first. |
| `search` | exactly one of `regex` / `substring`, plus `path` (scope), `glob`, `case_sensitive`, `max_results` (default 100) | Full-text content search via ripgrep. `regex` treats metacharacters as special, `substring` does not; smart-case unless `case_sensitive`; `glob` restricts by filename. Returns `file:line: text`. |
| `find_files` | at most one of `regex` / `glob`, plus `type` (`file`\|`dir`), `path` (scope), `max_results` (default 200), `from` — all optional | Find files/directories by name via fd. `regex` matches the filename as a regular expression, `glob` as a shell pattern; neither lists everything under the scope. Returns a sorted, paginated path list. |
| `search_frontmatter` | `filters`, `facets`, `path` (scope), `sort` (`path`\|`modified`), `sort_reverse`, `max_results` — all optional | Find notes by their YAML frontmatter values, and summarise those values with facets. Each operator names the type it reads; facets are computed over the whole match set. |
| `read_lines` | `path`, `start` (default 1), `end` (default EOF) | Read a 1-based inclusive line range, line-numbered. |
| `read_file` | `paths` (one or many), `cap` (bytes per file, optional) | Read whole text files — one entry per path, in order, so a set of search results can be pulled in a single call. A call returns at most 1 MiB in total. |
| `read_frontmatter` | `paths`, `cap` (bytes per file, default 2000) | Return the raw, unparsed YAML frontmatter block of each note — one entry per path, in order. |
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
  [ripgrep](https://github.com/BurntSushi/ripgrep): smart- or exact-case,
  optionally narrowed by `path` (a subtree) and `glob` (filenames, e.g. `*.md`,
  or `!*.log` to exclude). Returns line matches, capped by `max_results` and
  streamed so a broad query stops early rather than draining ripgrep.
- `find_files` — **name** discovery backed by
  [fd](https://github.com/sharkdp/fd): `type` file/dir, `path` scope. Returns a
  sorted, cursor-paginated list of paths.

Both name their matching mode in the field that carries the pattern, rather
than taking one query plus a mode flag: `search` takes exactly one of `regex`
or `substring`, and `find_files` at most one of `regex` or `glob` (neither
lists the whole scope). Naming both, or — for `search` — neither, is an error
that explains the difference. This is deliberate: a mode flag is read once when
the tool list is loaded and forgotten by the time a call is written, so a
literal pattern like `- [ ]` would silently compile to a valid regex meaning
something else and quietly return the wrong lines. A field called `substring`
cannot be filled in wrongly.

Both require their binary on `PATH` (`rg`, `fd`), confine the scope inside the
served folder, do not follow symlinks, and skip hidden files, `.git`, and
gitignored paths by default.

## Restarts and stale tool lists

Restarting kbmcp drops every session, so a client's next request gets
`404 session not found` and it reconnects. A client that caches the tool list
across that reconnect would keep calling parameters that no longer exist, so on
connect kbmcp sends `notifications/tools/list_changed`.

That notification carries no data — it only says the list is stale. The client
then calls `tools/list` itself, and the current definitions come back in that
response; MCP has no message that pushes tool definitions. It also only reaches
a client that is listening for it: a modern client must have subscribed via
`subscriptions/listen`, and a legacy one must hold its SSE stream open. A client
that does neither is unreachable, and reconnecting it is the only remedy.

A third, `search_frontmatter`, searches the structured header rather than the
prose — see below. LanceDB-backed semantic search is still planned, on the same
principle: one tool per result shape.

## Frontmatter

Notes carry a YAML frontmatter block, and two tools work on it. Unlike `search`
and `find_files` these are pure Go — no external binary, a full scan every time,
which is the right trade for a vault of hundreds of notes. If it ever outgrows
that, the answer is a real search engine, not a hand-rolled index.

### `read_frontmatter` — the raw block

Returns the bytes between the leading `---` delimiters, **unparsed**:

```
mails/fastmail_12299_How to have ideas.md
---
date: '2026-08-28T20:54:22+00:00'
from_name: Benn Stancil
subject: How to have ideas
---
```

Not parsing is the point. An agent reads YAML perfectly well, and the raw block
is the only view that shows *structure* — that `attachments` is a list of maps
with `filename`/`mime_type`/`size_bytes`, which any field-level projection
flattens away. It is also far cheaper than `read_file`: a mail's frontmatter is a
few hundred bytes against multiple KB of newsletter HTML.

`read_file` takes a list of paths too, so the natural follow-up to a search —
pulling the whole result set — is one call rather than one per hit. Both tools
report per-path problems (missing, a directory, binary) as flagged entries
instead of failing the batch, and `read_file` bounds a call to 1 MiB in total, so
a wide search cannot flood a context window by accident.

It takes `paths` and nothing else — no glob, no scope, no recursion. `list_files`
and `find_files` already select files, and a second, weaker selector here would
only be a shortcut that accumulates. `cap` bounds the bytes per file (default
2000); a block that is longer, or never closed, comes back with `truncated: true`
rather than an error, so an agent that cares simply asks again with a bigger cap.
Files with no frontmatter, missing paths and directories stay in the result as
flagged entries instead of silently vanishing.

### `search_frontmatter` — query and facets

`filters` are conditions that must all hold. **Every operator names the type it
reads**, so nothing is inferred and nothing is overloaded:

| Operators | Reads the value as |
|---|---|
| `exists` | any non-null value at the path |
| `text_eq`, `text_contains` | text (case-sensitive) |
| `bool_eq` | boolean |
| `eq`, `lt`, `lte`, `gt`, `gte` | number (numeric is the unprefixed default) |
| `date_eq`, `date_lt`, `date_lte`, `date_gt`, `date_gte` | whole days, UTC |
| `time_eq`, `time_lt`, `time_lte`, `time_gt`, `time_gte` | instants |

`date_` and `time_` are separate so that comparing a timestamp against a bare day
never has to invent an answer to "is `2026-01-03` midnight or 23:59?" — the
caller picks the granularity. A date operator accepts every spelling YAML
produces for one type (a quoted ISO string, a native YAML date, any UTC offset).

A field value that is not of the operator's type simply **does not match**; that
is ordinary in a loosely-typed vault, not an error. A *query* value of the wrong
type is the opposite — a caller bug, and it fails the call with a message saying
so. Field paths are dotted and descend maps **and lists**, so
`attachments.mime_type` reaches into a list of maps and matches if any element
fits.

`facets` summarise the whole match set — not just the returned page. The caller
chooses the statistic, for the same reason operators name their types:

| Stat | Returns |
|---|---|
| `text_top` | the most common values with counts (`n`, default 5) |
| `range` | numeric min/max |
| `date_range`, `time_range` | min/max |
| `date_bins`, `time_bins` | histogram over the full range plus min/max; `bin` is `day`/`month`/`year`, and for `time_bins` also `minute`/`hour` |

Bins are computed in **UTC** and empty bins are omitted, so a histogram can never
have more rows than there are matches. There is no `week` bin: alone among them
it carries a convention (ISO Monday vs Sunday) that would have to be guessed.

Every facet carries `count` (values seen), `docs` (documents having the field),
`distinct`, and `unparsed` (values that were not of the requested type). Those
denominators are the point — the tool reports numbers and never a verdict, so a
field whose `distinct` equals its `count` with top counts of 1 tells the agent
"opaque identifier" without this code ever classifying anything. `count` exceeds
`docs` when a field holds a list, since each element counts.

Results carry `scanned`, `with_frontmatter` and `unparsable` alongside `total`,
which is what makes an empty result interpretable: no matches among 105 notes
that have frontmatter means something quite different from no matches among none.
A note whose YAML is malformed is counted in `unparsable` and skipped — one bad
note must not fail a query over the whole vault.

```jsonc
// "legitimate mail since Sep 2, newest file first"
{
  "path": "mails",
  "filters": [
    {"field": "authentication.verdict", "op": "text_eq",   "value": "legitimate"},
    {"field": "date",                   "op": "date_gte",  "value": "2026-09-02"}
  ],
  "facets": [{"field": "from_name", "stat": "text_top", "n": 5}],
  "sort": "modified", "sort_reverse": true, "max_results": 20
}
```

Pass `max_results: 0` for facets only, with no document list — the probe an agent
wants when asking "what is even in here?". Sorting is by `path` or `modified`;
note that for imported notes `modified` is *import* time, not the note's own
date. Sorting by a frontmatter field is not supported yet, and neither are
negation, regex matching, or relative date bounds — all additive, and left until
real usage asks for them.

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

In HTTP mode the server also exposes a small REST surface alongside the MCP
endpoint, behind the same bearer token — enough for a plain editor front-end that
doesn't speak MCP:

| Method | Path | Body | Description |
|--------|------|------|-------------|
| `GET` | `/files/<path>` | — | A file's raw content (markdown as `text/markdown`), or a JSON listing for a directory. |
| `PUT` | `/files/<path>` | raw file content | Create or overwrite the file, then commit it. Returns `201`/`200`, an `ETag`, and a small JSON body. |
| `GET` | `/history` | — | Recent commits as JSON (`{"commits": [...]}`), mirroring the `history` tool. Optional `?max=&path=&since=`. |

`PUT` carries commit metadata in the query string, so the body stays pure
content: `?message=…` (**required**) plus optional `?author_name=…&author_email=…`.
It commits through the exact same path as `write_file`.

Conditional requests give agents optimistic locking against each other, using the
`ETag` from a prior `GET`:

- `If-Match: <etag>` — overwrite only if the file is unchanged, else `412`.
- `If-None-Match: *` — create only; `412` if the file already exists.

```sh
curl -X PUT 'http://host:8070/files/notes/idea.md?message=Add+idea&author_name=alice&author_email=alice@x' \
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

Serves the same MCP tools and the REST surface over Streamable HTTP. A shared
bearer token is **always required**: every request must carry
`Authorization: Bearer <token>` (compared in constant time; anything else gets a
`401`, logged with the reason and the caller's address). Set it via `$KBMCP_TOKEN`
(preferred — it keeps the token out of the process list; it can also come from
the `.env` file, see below) or `-token`. There is no unauthenticated HTTP mode;
for a local server without auth, use stdio.

```sh
KBMCP_TOKEN=your-secret ./kbmcp -http :8070 /path/to/folder
```

`-http` takes a full address. Given only a port, the server binds **loopback**
(`127.0.0.1`) — the intended deployment, with a reverse proxy on the same host as
the sole ingress. Pass a host explicitly when the gateway lives elsewhere, e.g.
`-http 100.x.y.z:8070` to bind a [Tailscale](https://tailscale.com) address:

```sh
./kbmcp -http 100.x.y.z:8070 /path/to/folder   # reachable from a gateway on another host
```

**Behind an auth gateway.** The expected setup is a fronting proxy (Caddy
`forward_auth` → an OAuth server) that authenticates the real user, then forwards
to kbmcp with the shared token plus `X-Volume-User` / `X-Volume-Scopes` headers.
The token is what makes those headers trustworthy — it proves the caller is the
gateway and not a client that reached the port directly. kbmcp logs the injected
identity but does not itself enforce scopes.

**Network safety:** traffic is plain HTTP, so the bearer token and file contents
are unencrypted on the wire. Keep the listener on loopback or a trusted network
(home LAN, tailnet) and terminate TLS at the reverse proxy; there is no built-in
TLS.

### Configuration

Config resolves flags first, then the environment, then an env file:

| Flag | Meaning |
|------|---------|
| `-http <addr>` | serve over HTTP instead of stdio (host defaults to loopback) |
| `-token <token>` | bearer token; prefer `$KBMCP_TOKEN` |
| `-env <path>` | env file to load at startup (default `.env`) |

The env file holds simple `KEY=VALUE` lines (`#` comments, optional `export`,
optional quotes). Real environment variables always win, and a missing file is
not an error — it's a convenience for `KBMCP_TOKEN`, not a config system.

## Logs

The server logs every request to stderr: each tool call with its name, a
(truncated) summary of its arguments, whether it succeeded or `FAILED` (with the
error message), and how long it took. Other methods (`initialize`, `tools/list`)
are logged briefly.

Every line is prefixed with the session id (first 8 characters) and, when the
client announced one, its name. Several clients typically share a vault — agents
alongside an editor — and behind the gateway they all arrive as the same user,
so the session is what tells them apart. A client using the modern
`server/discover` handshake sends no name and is identified by session alone.

```
kbmcp: [-] POST / user="mikio" scopes="read write"
[2O2XS7X5 editor-spa] initialize (0s)
[2O2XS7X5 editor-spa] initialized; sent tools/list_changed
[2O2XS7X5 editor-spa] tools/list (0s)
[2O2XS7X5 editor-spa] tool search {"substring":"alpha"} -> ok (2ms)
[2O2XS7X5 editor-spa] tool edit_file {"path":"notes.md","old_string":"foo",...} -> FAILED: old_string not found in notes.md (1ms)
```

The gateway line carries the same id, so HTTP-level and MCP-level lines for one
client line up; `[-]` is a request made before a session exists, which is the
`initialize` or `server/discover` itself.

Watch them live by running the server in a terminal, or — once it runs under
systemd — with `journalctl -fu kbmcp`.

## What a client is told at connect

Beyond the per-tool descriptions, the server returns a short `instructions`
string on both handshakes (`initialize` and `server/discover`). It points the
client at `README.md` in the **served folder** — the vault's own README, which
describes how that particular knowledge base is organised — and covers the
cross-tool things no single tool description can: which tool answers which kind
of question, that the read tools take lists so results should be pulled in one
call, and that history is queryable because every write is a commit.

It is spent from every client's context on every connection, so it is kept
short and deliberately does not restate the tool descriptions.

The text lives in `INSTRUCTIONS.md`, read once at startup, so whoever curates a
vault can edit it as prose without a rebuild. Point `-instructions` elsewhere to
serve a different file — useful when one binary serves more than one vault, since
the text describes a *particular* knowledge base. A missing file is not an error:
the server simply sends no instructions. Edits take effect on restart, and a
connected client will not see them until it reconnects.

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
claude mcp add --transport http kb http://your-host:8070/ \
  --header "Authorization: Bearer your-secret"
```

```json
{
  "mcpServers": {
    "kb": {
      "url": "http://your-host:8070/",
      "headers": { "Authorization": "Bearer your-secret" }
    }
  }
}
```

## Notes / not-yet-done

- HTTP auth is a single shared bearer token. Per-user identity comes from a
  fronting gateway (which can add OAuth) as injected headers; the server logs
  that identity but does not enforce scopes itself.
- No built-in TLS — terminate TLS at a reverse proxy for internet exposure.
- `search` and `find_files` shell out to `rg` and `fd` — both must be installed
  on the host. The frontmatter tools have no such dependency.
- Single served folder per process.
