# fmq — frontmatter query

This describes fmq's behaviour, as implemented by the Go version in this
repository. Where the behaviour still comes from a Go library and not from a
decision, the spec says so: another implementation must reproduce it to be
conformant.

fmq scans directory trees, parses the YAML frontmatter of each file, keeps the
files whose frontmatter satisfies every filter, and summarises the matching set
with facets.

**Conformance:** `conformance/` holds this spec as executable cases (a vault, a
query, the expected result), run against any binary:

```sh
FMQ=/path/to/fmq go test ./cmd/fmq -run Conformance
```

The format of a case is described in `conformance_test.go`. Error messages are
not part of the contract yet; an error case only checks for exit status 2.

## Invocation

```
fmq [options] [filters] [dir...]
```

The **query document** (below) is the canonical input and the **result
document** the canonical output. Programs use those:

```sh
fmq -query-json - -o json -- notes < query.json
```

- `-query-json FILE` reads the query from FILE, `-` for stdin. The document
  must be a single JSON object; unknown keys are an error.
- `-o text|json` selects the output: the result document as one line of JSON,
  or text (the default). In text, **stdout holds only the answer**, so it can
  feed `xargs` or a loop: each matching path followed by a newline (a NUL with
  `-0`), then — if facets were asked for — a blank line and the facets. The
  summary line (the counts, and "showing the first N" when truncated) goes to
  stderr. The wording of the summary and of the facet blocks is not a contract.
- `-0` ends each path with NUL instead of newline, for `xargs -0`. It cannot be
  combined with facets or `-o json`, which would put other text on the stream.
- `dir...` are the directories to scan (a file is allowed too), default `.`.
  They are separate from the query, as with `rg` and `fd`.

For a shell, flags build the same query document instead of `-query-json`
(combining the two is an error). Filters are find-style: **every operator is a
flag**, and its operands are the following arguments.

| flag | query |
|---|---|
| `-exists FIELD`, `-not_exists FIELD` | appends `{"field": FIELD, "op": "exists"}` (or `not_exists`) |
| `-OP FIELD VALUE`, for every other op | appends `{"field": FIELD, "op": OP, "value": VALUE}` |
| `-f FIELD:STAT[:ARG]` | appends a facet. ARG is `n` for `text_top`, `bin` for `*_bins`, not allowed otherwise. |
| `-s path\|modified` | `sort` |
| `-r` | `sort_reverse` |
| `-n N` | `max_results` |
| `-field FIELD` | appends to `fields`. Only with `-o json`: field values have no text form. |

- Every flag has a fixed number of operands, taken as the next arguments
  verbatim — whatever they look like, so `-int_gt n -5` works. Quoting is the
  shell's alone: fmq never splits or unquotes an operand.
- `-eq -lt -lte -gt -gte` are refused, naming the `-int_` and `-float_` forms.
- Any other argument is a dir, in any position. `--` makes every argument after
  it a dir, for a dir whose name starts with `-`.
- `--flag` is accepted as `-flag`. There is no `-flag=value` form.
- Filters and `-f` repeat; for `-s -n -o -query-json` the last one wins.

**Exit status** is 0 on success — with or without matches — and 2 on any error,
with a one-line message on stderr prefixed `fmq: `. Callers relay the message:
it names the fix (e.g. `use int_lte`).

## Query

```json
{
  "filters":      [{"field": "date", "op": "date_gte", "value": "2026-01-01"}],
  "facets":       [{"field": "tags", "stat": "text_top", "n": 10}],
  "sort":         "modified",
  "sort_reverse": true,
  "max_results":  50,
  "fields":       ["title", "tags"]
}
```

All keys are optional.

| key | meaning |
|---|---|
| `filters` | conditions that must all hold (AND). Empty matches every file that has parsable frontmatter. |
| `facets` | summaries over the **whole** match set, not only the returned page |
| `sort` | `path` (default) or `modified` |
| `sort_reverse` | reverses the whole order |
| `max_results` | matches to return. Absent: all of them — a caller with a budget, such as an agent's context window, sets its own. `0` returns no matches (facets only). Negative is an error. |
| `fields` | field paths whose values to return with each match (see *Result*). Text output refuses them; use JSON. |

There is no pagination cursor yet.

### Filter

`{"field", "op", "value"}`. `value` is always a string; the operator decides how
it is read. `field` must not be empty.

### Facet

`{"field", "stat", "n", "bin"}`. `n` only with `text_top`, `bin` only with (and
required by) `date_bins` / `time_bins`. `field` must not be empty.

### Errors

The whole query fails, with a message naming the field, when:

- an op or stat is unknown. The untyped `eq` `lt` `lte` `gt` `gte` and `range`
  are refused with a message naming both typed replacements (`int_lte` /
  `float_lte`, `int_range` / `float_range`)
- a filter's `value` does not read as its operator's type (see *Conversions*)
- `bin` is missing, not allowed for the stat, or not one of the stat's bins
- `n` is given for a stat other than `text_top`, or is negative
- `sort` is not `path` or `modified`
- `max_results` is negative
- a `field` is empty
- a dir does not exist

A *field* value of the wrong type is not an error; it just does not match.

## The scan

1. Walk each dir recursively, in the order given. Dirs that overlap report a
   file once per dir that reaches it.
2. Skip every entry the walk finds whose name starts with `.`; a hidden
   directory is not descended into. A dir named explicitly is scanned even if
   hidden.
3. Consider only regular files. Symlinks are not followed and not counted.
4. Every considered file counts toward `scanned` — including non-markdown and
   binary files.
5. Read at most the first 1 MiB (1048576 bytes). A file that cannot be read, or
   whose first 512 bytes contain a NUL byte, has no frontmatter.

### Frontmatter block

- The file must start, at byte 0, with a line that is exactly `---`, optionally
  followed by `\r`. A leading BOM or whitespace means no frontmatter.
- The block ends at the first later line that is exactly `---` (optional `\r`).
  `...` is not a terminator.
- The block is the text between the two delimiter lines, delimiters excluded.
- A block not closed within the 1 MiB read limit is unterminated.
- A file with a block, terminated or not, counts toward `with_frontmatter`.

### Parsing

The block is parsed as YAML into a tree of **maps, lists, scalars and nulls**.
YAML's type resolution is not applied: every scalar is kept as its text, after
YAML's quoting and escapes are undone (`'a''b'` → `a'b`, a `|` block → its
lines). So `2026-01-03`, `'2026-01-03'` and `"2026-01-03"` are the same value,
and so are `017` and `'017'`. Tags (`!!str`, `!!int`, …) are ignored.

- **null:** a scalar that is unquoted (plain, possibly tagged) and whose text is
  empty, `~` or `null`. `Null`, `NULL` and any quoted form are text.
- **keys:** the key scalar's text; `1: x` has the key `1`. A non-scalar key
  makes the block unusable. `<<` is an ordinary key; there is no merge.
- **aliases:** `*x` expands to the value anchored as `&x`.

The block is **unusable** — counted toward `unparsable` and never matched — when:

- it is unterminated
- YAML rejects it
- its top level is not a mapping (a list or a bare scalar)
- a mapping repeats a key
- a key is not a scalar
- expanding it produces more nodes than the block has bytes (alias loops and
  alias bombs)

An empty block, or one holding only comments, is a mapping with no fields.

The YAML parser in use is `gopkg.in/yaml.v3` v3.0.1; what it accepts as valid
YAML is part of the contract until a conformance case pins it down.

## Field paths

`field` is split on `.`; each segment descends into a map by key. Lists are
descended implicitly at every level, recursively, so `attachments.mime_type`
reaches into each element of a list of maps and `[[a, b], c]` yields `a`, `b`,
`c`. A path therefore yields zero or more values; a missing key yields none. A
key containing `.` cannot be addressed. Nulls are included in the values.

A filter holds if **any** value at its path satisfies the operator. Each filter
picks its own value: `tags: [1, 20]` satisfies both `int_lt 5` and `int_gt 10`,
though no single element satisfies both. There is no per-element conjunction.

## Conversions

Each operator and stat names one target type and converts values to it. Only
scalars convert; nulls, maps and lists-as-a-whole never do. A value that does
not convert does not match (filters) or counts as unparsed (facets). The query's
`value` goes through the same conversion, where failure is an error.

Each type accepts its written form **exactly**: no trimming, no alternative
spellings.

**text** — the scalar's text.

**int** — text matching `^-?[0-9]+$`, read as an arbitrary-precision integer.
Leading zeros are allowed (`017` is 17). `5.0` and `1e3` are not integers.

**float** — text matching `^-?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?$`, read as a
64-bit float (rounded to nearest). A value whose exponent overflows float64
(`1e999`) does not convert. So no `NaN`, `Inf`, `+5`, `.5`, `5.`, hex or
`1_000`. Every int is also a float, and reading one as a float is lossy past
2^53: `float_eq 9007199254740993` matches `9007199254740992`. Use `int_` for
identifiers.

**bool** — the text `true` or `false`.

**time** — text parsed with the first of these Go `time.Parse` layouts that
accepts it; layouts without an offset are read as UTC, and the result is
converted to UTC:

```
2006-01-02T15:04:05.999999999Z07:00   (RFC 3339 with fraction)
2006-01-02T15:04:05Z07:00             (RFC 3339)
2006-01-02T15:04:05
2006-01-02 15:04:05
2006-01-02T15:04
2006-01-02 15:04
2006-01-02
2006/01/02
```

`2026-1-3` and YAML's spaced form `2026-01-03 10:00:00 +02:00` are not times.
Go's layout parsing (e.g. how many fraction digits it takes) is part of the
contract.

**date** — the *time* conversion, truncated to the whole UTC day. So
`2026-01-03T01:00:00+02:00` is the date 2026-01-02.

## Operators

| op | type | holds when |
|---|---|---|
| `exists` | — | some value at the path is non-null. An empty list has no values. |
| `not_exists` | — | `exists` does not hold: the path is missing, or has only nulls, or only empty lists. The one op that is not "any value satisfies". |
| `text_eq` | text | equal, byte for byte |
| `text_contains` | text | `value` is a substring, case-sensitive |
| `bool_eq` | bool | equal |
| `int_eq` `int_lt` `int_lte` `int_gt` `int_gte` | int | comparison holds, exactly |
| `float_eq` `float_lt` `float_lte` `float_gt` `float_gte` | float | comparison holds |
| `date_eq` `date_lt` `date_lte` `date_gt` `date_gte` | date | comparison holds |
| `time_eq` `time_lt` `time_lte` `time_gt` `time_gte` | time | comparison holds |

A value that is a map satisfies only `exists` (and so never `not_exists`).

## Facets

Facets are accumulated over every matched file, before `max_results` applies.
For each matched file with at least one value at the path (nulls included):

- `docs` +1
- `count` + the number of values
- each value that converts to **text** is tallied; `distinct` is the number of
  distinct text renderings, whatever the stat
- a value that does not convert to the stat's type counts toward `unparsed`
  (for `text_top`: nulls and maps)

| stat | adds |
|---|---|
| `text_top` | `top`: the `n` (default 5; `0` means default) most frequent text values, count descending, ties by value ascending in byte order |
| `int_range` | `min`, `max` as integers in canonical decimal (no leading zeros, `-0` → `0`) |
| `float_range` | `min`, `max` rendered by Go `strconv.FormatFloat(f, 'g', -1, 64)`: `1.0` → `1`, `1e21` → `1e+21` |
| `date_range` | `min`, `max` as `YYYY-MM-DD` |
| `time_range` | `min`, `max` as RFC 3339 UTC |
| `date_bins` | `min`, `max` as for `date_range`, plus `bins`; `bin` ∈ `day month year` |
| `time_bins` | `min`, `max` as for `time_range`, plus `bins`; `bin` ∈ `minute hour day month year` |

Bin keys are the UTC instant formatted as `2006-01-02T15:04` (minute),
`2006-01-02T15` (hour), `2006-01-02` (day), `2006-01` (month), `2006` (year).
Bins are sorted by key; empty bins are omitted. There are no week bins.

`min`/`max` are omitted when no value converted.

## Result

```json
{
  "matches": [{"path": "notes/a.md", "modified": "2026-09-29T10:00:00Z",
               "fields": {"title": ["Hello"], "tags": ["a", "b"]}}],
  "total": 1,
  "truncated": false,
  "scanned": 12,
  "with_frontmatter": 10,
  "unparsable": 1,
  "facets": [{
    "field": "tags", "stat": "text_top",
    "docs": 1, "count": 2, "distinct": 2, "unparsed": 0,
    "top": [{"value": "a", "count": 1}, {"value": "b", "count": 1}]
  }]
}
```

- `path`: the path as walked from its dir, lexically cleaned (`./notes/` →
  `notes/…`) and `/`-separated. Relative dirs give relative paths.
- `modified`: the file's mtime, UTC, RFC 3339, whole seconds. Empty string if it
  could not be read.
- `fields`: present only when the query asked for fields; one key per requested
  field path. Each value is **always a list**: exactly the values a filter on
  that path sees (*Field paths*), in document order — flattened through lists,
  `[]` when the path is missing. Scalars are their text, nulls are `null`, a map
  is a JSON object (scalars inside it are text too; lists inside it stay
  lists). `tags: a` and `tags: [a]` both give `["a"]`.
- `field_coverage`: present only when the query asked for fields. For each
  requested field, how many matches — **all of them**, not only the returned
  page — have a value for it, where "has a value" means exactly `exists`. Every
  requested field appears, `0` included: `0` against a non-zero `total` is how a
  misspelt or unused field shows.
- Order: `sort=path` is byte-wise by path. `sort=modified` is by the `modified`
  string, ties broken by path. `sort_reverse` reverses the result of either.
- `total`: all matches. `matches` is the first `max_results` of them after
  sorting; `truncated` is true when `total` exceeds that (so also with
  `max_results: 0` and at least one match).
- `matches` is always a list, `[]` when empty.
- `facets` is omitted when none were requested; `top`, `bins`, `min`, `max` are
  omitted when empty. Facets appear in request order.
