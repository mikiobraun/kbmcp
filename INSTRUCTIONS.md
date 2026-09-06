This server exposes one git-backed folder of markdown notes with frontmatter, which is optional YAML between --- fences at the beginning of a file. There are special tools to search and read frontmatter.

- Read README.md at the root of the served folder first. It describes how this
  particular knowledge base is organised — its folders, its naming and
  frontmatter conventions, and what belongs where. That is deliberately not
  repeated here, because it differs from vault to vault.
- Picking a tool:
  - `search` matches the text of notes
  - `search_frontmatter` matches the structured YAML header, and can summarise a whole match set with facets
  - `find_files` matches filenames
  - `read_frontmatter` shows a note's frontmatter without the body
  - `list_files` browses the tree when you have no term to search for
- Reading notes:
  - `read_file` and `read_frontmatter` take a list of paths and return one entry per path — pull a whole set of search results in one call, not one call per file
  - `read_lines` reads a line range inside a single file
  - results are capped; when one comes back truncated, pass `next_from` back for the next page rather than broadening the query
- Following links:
  - `outgoing_links` lists the [[wiki links]] in a note, and which of them are broken
  - `backlinks` finds the notes pointing at one
  - `orphans` lists notes that nothing links to
- Writing, where every change is a git commit:
  - `write_file` replaces a whole note, `edit_file` replaces a string inside one
  - `batch_edits` applies several edits as one commit, and writes nothing at all if any single edit fails
- Reading history, which is queryable data rather than bookkeeping:
  - `history` lists recent commits, `diff` shows what one changed
  - `file_at` reads a note as it stood at a revision
- Paths are relative to the served folder, and dotfiles are invisible.
