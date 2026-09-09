# Backlog

Known work that is deliberately not done yet, with the reasoning that led there.
An entry earns its place by being a decision someone would otherwise have to
re-derive — not by being a to-do.

## REST writes are attributed to the vault's git identity, not the caller

`author_email` is required of the writing *tools*, so an agent always names
itself. The REST `PUT` keeps both author fields optional, and when they are
omitted git falls back to the identity configured in the vault repo — so every
write through the editor is attributed to whoever set the vault up, whatever
session it arrived on.

That is fine for a single-user vault and wrong the moment there are two. The
information is already there and unforgeable: `X-Volume-User` reaches kbmcp on
every gateway-authenticated request (volume-auth sets it after verifying the
JWT), and today only `logIdentity` reads it.

**Why it isn't done:** the subject is a *username* (`mikio`), and volume-auth
holds no email for a user — its config is `users: - username: …`. Attribution
would either synthesise an address, or carry a real one end to end:

1. volume-auth: an `email:` field per user, emitted as `X-Volume-Email`
2. dev-router: `copy_headers` is hard-coded to `X-Volume-User X-Volume-Scopes`
   and would need the treatment `cors.headers` got — configurable, not fixed
3. kbmcp: use the header as the author when the REST call doesn't specify one
4. editor: nothing — which is the point of doing it this way rather than having
   the browser assert an identity the gateway has already established

Four repos for a change that buys nothing until there is a second writer, so it
waits for one.

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
