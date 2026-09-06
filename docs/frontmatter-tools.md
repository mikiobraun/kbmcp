# Frontmatter tools — spec

Two tools, designed separately: `read_frontmatter` (return the raw material) and
a frontmatter query tool (still under design, see the end of this document).

Design stance for both: **return the material, let the calling agent decide.**
The tools do mechanism, not judgment — no guessing what the caller meant, no
inferring types, no pre-digesting content the agent can read perfectly well
itself. Where a bound is needed it is a caller-supplied parameter with a boring
default, never a rule that silently decides semantics.

---

## `read_frontmatter` — implemented (`frontmatter.go`)

Return the raw YAML frontmatter block of each requested file, uninterpreted.

### Why raw, unparsed

- The agent reads YAML fine. Parsing to JSON and back loses nothing useful and
  costs a dependency, an error surface, and fidelity (comments, key order,
  original date spellings, malformed-but-readable blocks).
- Structure is the point. A field-level query flattens `attachments` into
  `attachments.mime_type`; only the raw block shows that it is a **list of maps**
  with `filename`/`mime_type`/`size_bytes` — which is what you need to know
  before writing a query against it.
- Context economy: a mail's frontmatter is a few hundred bytes, its body is
  multiple KB. `read_file` on ten mails to see their headers is mostly wasted
  newsletter HTML.
- Zero dependencies. This tool is pure byte slicing and ships independently of
  anything that needs a YAML parser.

### Input

| Field | Type | Required | Description |
|---|---|---|---|
| `paths` | `[]string` | yes | Files to read, relative to the served root. Order is preserved in the output. At most 1000 per call. |
| `cap` | `int` | no | Maximum bytes of frontmatter returned **per file**. Default 2000. Clamped to `maxFileBytes` (1 MiB). |

Paths go through the same `resolve()` confinement as every other tool: absolute
paths, `../` escapes, and hidden paths are rejected — and those reject the whole
call, since they indicate a caller bug rather than an odd file.

There is deliberately **no** `path` scope, `glob`, or `recursive` selection here.
`list_files` and `find_files` already select files, with pagination. A second,
weaker selector inside this tool would be a shortcut that accumulates. The cost
is that "sample three mails" is two calls.

### Extraction rule

Fully determined; no judgment at runtime.

1. The file must begin with `---` at byte 0, followed by a line ending. Anything
   else (including a leading blank line or BOM) means no frontmatter.
2. The block ends at the first subsequent line that is exactly `---`.
   Only `---` — `...` (a legal YAML document end) is not recognised, because no
   real file has ever been seen using it here; such a file reads as unterminated.
3. The returned `frontmatter` is the **verbatim byte slice** between the two
   delimiter lines. Delimiters are excluded. Nothing is trimmed, reordered, or
   normalised.
4. Delimiter lines are matched after stripping a trailing `\r`, so CRLF files
   work. The returned block keeps whatever line endings it had.
5. Scanning stops once the block would exceed `cap` bytes. The bound is the
   *block* offset, not bytes scanned: a delimiter starting at exactly `cap`
   closes a block of exactly `cap` bytes, which fits and is complete. If the closing delimiter was found within
   that budget, the block is complete (`truncated: false`). If not — whether
   because the block is longer than `cap` or because the file never closes it —
   the first `cap` bytes are returned with `truncated: true`. These are the same
   case and are not distinguished; an agent that cares re-reads with a larger
   `cap`.
6. Truncation cuts at `cap` bytes, backing off only far enough to avoid splitting
   a UTF-8 rune. It does **not** back off to a line boundary: a partial last line
   is honest, and `truncated` says so.

Consequences worth stating: an unterminated `---` is not an error, a malformed
YAML block is returned as-is (this tool never parses, so it cannot know), and a
file whose frontmatter is empty (`---\n---\n`) returns
`has_frontmatter: true, frontmatter: ""`.

### Output

Exactly one entry per input path, at the same index — duplicates in, duplicates
out. Never dropped, never reordered.

```json
{
  "entries": [
    {"path": "mails/x.md", "has_frontmatter": true,  "frontmatter": "date: '2026-08-28T20:54:22+00:00'\nsubject: How to have ideas\n", "truncated": false},
    {"path": "HELLO.md",   "has_frontmatter": false, "frontmatter": "", "truncated": false},
    {"path": "gone.md",    "has_frontmatter": false, "frontmatter": "", "truncated": false, "error": "no such file"}
  ]
}
```

`error` is present only when the file could not be read at all — `no such file`,
`is a directory`, or `appears to be a binary file`. Messages never include the
absolute path, matching every other tool. It exists so a typo'd path is distinguishable from a file
that genuinely has no frontmatter; without it both look identical and the agent
cannot tell. Per-entry, so one bad path does not fail the batch.

Text rendering: each entry as its path on a line, then the block wrapped in its
`---` delimiters (so block boundaries are unambiguous even though the structured
field excludes them), blank line between entries. Files with no frontmatter
render as `(no frontmatter)`, errors as `(error: …)`.

```
mails/x.md
---
date: '2026-08-28T20:54:22+00:00'
subject: How to have ideas
---

HELLO.md
(no frontmatter)
```

### Not in scope

No parsing, no validation, no type coercion, no field projection, no filtering,
no file selection, no sorting. Those either belong to the query tool or to the
agent.

---

## `search_frontmatter` — implemented (`frontmatter_search.go`)

As built:

- **No overloaded operators.** An operator names exactly one target type and
  converts the field value to it; a value that does not convert simply does not
  match. There is no inference, no fallback ladder, no "figure out what this
  could be."
- **`num_` is the default** (bare `lte`, `gte`, `lt`, `gt`, `eq` are numeric).
  Other types carry a prefix: `text_`, `bool_`, `date_`, `time_`.
- **`date_` and `time_` are separate** rather than one operator with a
  granularity rule. Comparing a timestamp against a day is ambiguous — is
  `2026-01-03` midnight or 23:59:59? — so the caller picks the granularity by
  choosing the operator, instead of the tool applying an implicit expansion.
- A date operator accepts the several *spellings* YAML produces for one type
  (quoted ISO string, native YAML date, varying UTC offsets). One declared target
  type, several input encodings — not a coercion ladder.
- **Sorting** reuses `list_files` verbatim: `sort: "path"|"modified"` plus
  `sort_reverse`, with the same `entrySortKey` composite cursor. Sorting by a
  *frontmatter field* is deferred — it would need the type declared the way the
  operators do (`sort: {field: date, as: time}`). Note `modified` is the file's
  mtime, which for imported mail is import time, not the mail's date; that is the
  importer's business, not this tool's.
- **No filename filter.** `find_files` covers name patterns; scoping here is the
  `path` subtree only.
- **Full scan, no index**, for the foreseeable future — this is hundreds of
  documents. If it ever outgrows that, the answer is a real search engine, not a
  hand-rolled index.
- Constraints are ANDed. Facets, sorting, projection, `@keys` inventory and
  regex matching are all deferred: they are additive, and real usage should
  decide them. The request/response *shape* is the expensive part to change and
  is what needs to be right first.
