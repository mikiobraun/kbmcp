package main

// Moving a file is the one write that changes what other files mean: a wiki
// link names its target rather than containing it, so a note that moves can
// leave links behind that now point elsewhere, or nowhere. A move therefore
// carries one rule, and nothing cleverer than it:
//
//	every link that resolved before the move resolves to the same note after
//	it — the moved note counting as itself at its new path.
//
// That covers more than links *to* the moved note. The note's own relative links
// ([[./x]], or a name found beside it) resolve from its new folder, and a third
// note's link can change without ever having pointed at it: a name the move
// brings into a folder shadows the vault-wide note that name used to find, and a
// name the move duplicates makes a unique name ambiguous. So the check is not a
// list of cases but the rule itself — resolve every link against the vault
// before and after, and rewrite exactly those whose answer changed. A link that
// was already broken has no target to keep, and is left alone.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// errTargetExists marks a move refused because the destination is taken. A
// move never overwrites: what is there would be lost to more than the history
// of one file.
var errTargetExists = errors.New("something already exists at the destination")

// LinkRewrite is one [[...]] a move respelled so that it still reaches the note
// it reached before.
type LinkRewrite struct {
	Path string `json:"path"` // note holding the link, at its path after the move
	Line int    `json:"line"`
	Old  string `json:"old"` // the link as written, brackets included
	New  string `json:"new"`
}

func newBatchState() *batchState {
	return &batchState{files: map[string]*batchFile{}}
}

// move moves the file at from to to within the batch, rewriting links as the
// rule above demands. Nothing is written; commit does that.
func (s *batchState) move(ctx context.Context, from, to string) error {
	fromAbs, err := resolveLeaf(from)
	if err != nil {
		return err
	}
	toAbs, err := resolveLeaf(to)
	if err != nil {
		return err
	}
	if fromAbs == toAbs {
		return fmt.Errorf("from and to are the same path")
	}
	fromRel, toRel := relPath(fromAbs), relPath(toAbs)

	src, err := s.moveSource(fromAbs)
	if err != nil {
		return err
	}
	before, err := s.view(ctx)
	if err != nil {
		return err
	}
	after := before.changed([]string{fromRel}, []string{toRel})
	for dir := filepath.Dir(toRel); dir != "."; dir = filepath.Dir(dir) {
		if _, isFile := after.files[dir]; isFile {
			return fmt.Errorf("%s is a file, so nothing can be moved inside it", dir)
		}
	}
	if f, seen := s.files[toAbs]; seen && f.exists {
		return fmt.Errorf("%s: %w", toRel, errTargetExists)
	} else if !seen {
		// ENOTDIR means a file on disk sits where a folder on the way should be;
		// the check above found none left in the batch, so it is being removed.
		if _, err := os.Lstat(toAbs); err == nil {
			return fmt.Errorf("%s: %w", toRel, errTargetExists)
		} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return err
		}
	}
	// The link would dangle, and a symlink's target is a path we would have to
	// rewrite on disk — a different kind of rewrite, for a case that is rare.
	for p, target := range before.files {
		if target == fromRel && p != fromRel {
			return fmt.Errorf("%s is the target of the symlink %s, which moving it would break", fromRel, p)
		}
	}

	moved := func(p string) string {
		if p == fromRel {
			return toRel
		}
		return p
	}
	type edit struct {
		link wikiLink
		raw  string
	}
	var rewrites []LinkRewrite
	texts := map[string]string{} // note path before the move -> rewritten content
	var touched []string         // the same notes, in vault order
	for _, note := range before.notes() {
		text, ok := s.text(note)
		if !ok {
			continue
		}
		at := moved(note)
		var edits []edit
		for _, l := range parseLinks([]byte(text)) {
			target := resolveTarget(l.Raw, note, before).Path
			if target == "" {
				continue
			}
			want := moved(target)
			if resolveTarget(l.Raw, at, after).Path == want {
				continue
			}
			raw, ok := rewriteLink(l.Raw, at, want, after)
			if !ok {
				return fmt.Errorf("%s:%d: [[%s]] cannot be written so that it still reaches %s", at, l.Line, l.Raw, want)
			}
			edits = append(edits, edit{l, raw})
			rewrites = append(rewrites, LinkRewrite{Path: at, Line: l.Line, Old: "[[" + l.Raw + "]]", New: "[[" + raw + "]]"})
		}
		if len(edits) == 0 {
			continue
		}
		// Back to front, so each splice leaves the earlier offsets valid.
		for i := len(edits) - 1; i >= 0; i-- {
			e := edits[i]
			text = text[:e.link.Start] + e.raw + text[e.link.End:]
		}
		texts[note] = text
		touched = append(touched, note)
	}

	for _, note := range touched {
		if note == fromRel {
			src.cur = texts[note]
			continue
		}
		f, err := s.load(note)
		if err != nil {
			return err
		}
		// Committing the rewrite commits the whole file, so a pending edit of
		// someone else's would go into this commit under this message.
		if f.existed {
			if err := requireCommitted(filepath.Join(root, note)); err != nil {
				return err
			}
		}
		f.cur = texts[note]
	}

	dst, seen := s.files[toAbs]
	if !seen {
		dst = &batchFile{}
		s.track(toAbs, dst)
	}
	dst.cur, dst.exists = src.cur, true
	if !dst.existed {
		switch {
		case src.existed:
			dst.orig, dst.movedFrom = src.orig, fromAbs
		case src.movedFrom != "":
			dst.orig, dst.movedFrom = src.orig, src.movedFrom
		}
	}
	src.cur, src.exists, src.movedFrom = "", false, ""
	s.links = append(s.links, rewrites...)
	return nil
}

// moveSource returns the state of the file a move takes, refusing what a move
// can't carry: a folder, a symlink (a relative target would stop pointing where
// it did), a binary file, and content git doesn't hold — the commit would sweep
// it in under the move's message.
func (s *batchState) moveSource(abs string) (*batchFile, error) {
	rel := relPath(abs)
	if f, seen := s.files[abs]; seen {
		if !f.exists {
			return nil, fmt.Errorf("%s: no such file: %w", rel, fs.ErrNotExist)
		}
		if f.existed {
			if err := requireCommitted(abs); err != nil {
				return nil, err
			}
		}
		return f, nil
	}
	info, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: no such file: %w", rel, fs.ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	switch {
	case info.IsDir():
		return nil, fmt.Errorf("%s is a directory; only files can be moved", rel)
	case info.Mode()&os.ModeSymlink != 0:
		return nil, fmt.Errorf("%s is a symlink; moving one is not supported", rel)
	}
	if err := requireCommitted(abs); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	if looksBinary(data[:min(len(data), 512)]) {
		return nil, fmt.Errorf("%s appears to be a binary file", rel)
	}
	f := &batchFile{orig: string(data), cur: string(data), existed: true, exists: true}
	s.track(abs, f)
	return f, nil
}

// view is the vault as the ops so far have left it.
func (s *batchState) view(ctx context.Context) (*vaultView, error) {
	disk, err := loadVault(ctx)
	if err != nil {
		return nil, err
	}
	var removed, added []string
	for p, f := range s.files {
		if f.exists {
			added = append(added, relPath(p))
		} else {
			removed = append(removed, relPath(p))
		}
	}
	return disk.changed(removed, added), nil
}

// text returns a note's content as the ops so far have left it, and false for
// one that is gone or can't be read as text.
func (s *batchState) text(rel string) (string, bool) {
	p := filepath.Join(root, rel)
	if f, seen := s.files[p]; seen {
		return f.cur, f.exists
	}
	data, err := os.ReadFile(p)
	if err != nil || looksBinary(data[:min(len(data), 512)]) {
		return "", false
	}
	return string(data), true
}

// rewriteLink respells raw so that, from the note at from, it resolves to want
// in v. It keeps what can be kept — surrounding spaces, the |alias, #heading or
// ^block suffix, an explicit .md — and tries forms in order, taking the first
// that resolves to want: the form the link already had, then a bare name, then
// a vault path. Each candidate is checked by resolving it, never assumed.
//
// A path with a leading slash reaches any file, so this fails only when want's
// name can't be written inside a link at all.
func rewriteLink(raw, from, want string, v *vaultView) (string, bool) {
	body, suffix := raw, ""
	if i := strings.IndexAny(raw, "|#^"); i >= 0 {
		body, suffix = raw[:i], raw[i:]
	}
	target := strings.TrimSpace(body)
	lead := body[:len(body)-len(strings.TrimLeftFunc(body, unicode.IsSpace))]
	trail := body[len(strings.TrimRightFunc(body, unicode.IsSpace)):]

	// An explicit .md stays; a link that left it off keeps leaving it off. A
	// file that isn't a note has to be named in full.
	spell := func(p string) string {
		if strings.HasSuffix(target, ".md") {
			return p
		}
		return strings.TrimSuffix(p, ".md")
	}

	var forms []string
	switch {
	case strings.HasPrefix(target, "./"):
		dir := filepath.Dir(from)
		if dir == "." {
			forms = append(forms, "./"+spell(want))
		} else if below, ok := strings.CutPrefix(want, dir+"/"); ok {
			forms = append(forms, "./"+spell(below))
		}
	case strings.HasPrefix(target, "/"):
		forms = append(forms, "/"+spell(want))
	case strings.Contains(target, "/"):
		forms = append(forms, spell(want))
	}
	forms = append(forms, spell(filepath.Base(want)), spell(want), "/"+spell(want))

	for _, f := range forms {
		// Brackets would end the link early, a backtick could turn it into
		// code, and a newline would split it.
		if strings.ContainsAny(f, "[]`\n") {
			continue
		}
		if resolveTarget(f, from, v).Path == want {
			return lead + f + trail + suffix, true
		}
	}
	return "", false
}

func renderLinks(links []LinkRewrite) string {
	if len(links) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d link(s) rewritten to keep their target:\n", len(links))
	for _, l := range links {
		fmt.Fprintf(&b, "%s:%d: %s -> %s\n", l.Path, l.Line, l.Old, l.New)
	}
	return b.String()
}

// ---- move_file ----

type MoveFileInput struct {
	From        string `json:"from" jsonschema:"file to move, relative to root"`
	To          string `json:"to" jsonschema:"path to move it to, relative to root; nothing may exist there yet, and missing folders are created"`
	Message     string `json:"message" jsonschema:"commit message (required); the move and every rewritten link are one commit"`
	AuthorName  string `json:"author_name,omitempty" jsonschema:"name to attribute the commit to; defaults to the part of author_email before the @"`
	AuthorEmail string `json:"author_email" jsonschema:"email to attribute the commit to (required): who or what is making this change, e.g. the agent's own address. Every commit names an author, so an automated writer stays distinguishable from a person"`
	DryRun      bool   `json:"dry_run,omitempty" jsonschema:"if true, check the move and return the links it would rewrite and the diffs, without writing or committing anything"`
}

type MoveFileOutput struct {
	From      string            `json:"from"`
	To        string            `json:"to"`
	DryRun    bool              `json:"dry_run"`
	Committed bool              `json:"committed"`
	Links     []LinkRewrite     `json:"links"`
	Files     []BatchFileResult `json:"files"`
}

// moveOutput is what move_file and POST /move report; from and to are the
// caller's, already accepted by move.
func moveOutput(s *batchState, from, to string, dryRun bool) MoveFileOutput {
	fromAbs, _ := resolveLeaf(from)
	toAbs, _ := resolveLeaf(to)
	b := s.output("", dryRun)
	out := MoveFileOutput{From: relPath(fromAbs), To: relPath(toAbs), DryRun: dryRun, Links: b.Links, Files: b.Files}
	// A JSON null would make "nothing to rewrite" awkward for every caller.
	if out.Links == nil {
		out.Links = []LinkRewrite{}
	}
	return out
}

func MoveFile(ctx context.Context, req *mcp.CallToolRequest, in MoveFileInput) (*mcp.CallToolResult, MoveFileOutput, error) {
	if strings.TrimSpace(in.Message) == "" {
		return nil, MoveFileOutput{}, fmt.Errorf("message is required")
	}
	s := newBatchState()
	if err := s.move(ctx, in.From, in.To); err != nil {
		return nil, MoveFileOutput{}, err
	}
	out := moveOutput(s, in.From, in.To, in.DryRun)

	status := "dry run, nothing written"
	if !in.DryRun {
		authorName, authorEmail, err := toolAuthor(in.AuthorName, in.AuthorEmail)
		if err != nil {
			return nil, MoveFileOutput{}, err
		}
		if out.Committed, err = s.commit(in.Message, authorName, authorEmail); err != nil {
			return nil, MoveFileOutput{}, err
		}
		status = "committed"
		if !out.Committed {
			status = "no changes to commit"
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "moved %s to %s — %s.\n", out.From, out.To, status)
	b.WriteString(renderLinks(out.Links))
	for _, f := range out.Files {
		if f.Diff != "" {
			fmt.Fprintf(&b, "\n--- %s ---\n%s", f.Path, f.Diff)
		}
	}
	return textResult("%s", b.String()), out, nil
}
