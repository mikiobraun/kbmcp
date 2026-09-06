# Backlog

Known work that is deliberately not done yet, with the reasoning that led there.
An entry earns its place by being a decision someone would otherwise have to
re-derive — not by being a to-do.

## Hidden-file filtering lives in two places

`isHidden` (`safe.go`) decides what kbmcp's own walkers skip: `restListDir`, the
list and read tools, `wikilinks`, `frontmatter_search`. Search and find don't use
it — they inherit `rg`'s and `fd`'s built-in hidden-file rules, since we pass
`--no-ignore` but never `--hidden`.

The two agree today only because both mean "a leading dot". They are not the same
implementation, and nothing enforces that they stay in step.

**So:** the moment `isHidden` grows a rule the tools can't express — hiding
`_drafts/`, a `*.tmp` pattern, a `.kbignore` file — search and find will surface
paths the file listing hides, and the vault will answer differently depending on
which door you came through. That is the same failure the `--no-ignore` change
removed for ignore files.

**Fix when it happens:** post-filter `searchCore`'s matches and `findCore`'s
paths through `isHidden`, applied to *every path component* rather than the
basename — a file inside a hidden directory is hidden, which is currently `rg`
and `fd`'s doing, and the easy part to get wrong when replicating it. Better
still, make `isHidden` the only implementation and post-filter from the start,
rather than keeping two rules that must be kept identical by hand.

Not built now: nothing needs it, and speculative machinery would be a third thing
to keep in step.
