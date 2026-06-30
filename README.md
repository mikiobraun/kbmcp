# kbmcp

A small [MCP](https://modelcontextprotocol.io) server (stdio) written in Go that
exposes a single folder on your filesystem for browsing, substring search, and
targeted reads. Built on the official
[`go-sdk`](https://github.com/modelcontextprotocol/go-sdk).

## Tools

| Tool | Arguments | Description |
|------|-----------|-------------|
| `list_files` | `path` (optional), `recursive` (optional) | List entries under the served folder. |
| `search` | `query`, `path` (optional scope), `max_results` (optional, default 100) | Case-insensitive substring search across text files. Returns `file:line: text`. |
| `read_lines` | `path`, `start` (default 1), `end` (default EOF) | Read a 1-based inclusive line range, line-numbered. |
| `read_file` | `path` | Read a whole text file (truncated at 1 MiB). |
| `write_file` | `path`, `content`, `dry_run` (optional) | Create or overwrite a file; parent folders are created. Returns a diff. |
| `edit_file` | `path`, `old_string`, `new_string`, `replace_all` (optional), `dry_run` (optional) | Replace exact text; `old_string` must be unique unless `replace_all`. Returns a diff. |

All paths are relative to the served folder. Requests that escape the folder
(`../`), absolute paths, and binary files are refused.

Both write tools return a unified-style diff of the change, and accept
`dry_run: true` to preview that diff without writing anything. Note that the
approval/confirmation prompt for a write is shown by the **client** (e.g. Claude
Desktop's "allow tool" dialog), not by this server — MCP has no server-rendered
diff-approval UI.

## Build

```sh
go build -o kbmcp .
```

## Run

### stdio (default — local, launched by the client)

```sh
./kbmcp /path/to/folder      # defaults to the current directory
```

The server speaks JSON-RPC over stdin/stdout — meant to be launched by an MCP
client, not run interactively.

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
