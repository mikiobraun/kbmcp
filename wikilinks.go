package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// wikiLinkRe matches an Obsidian-style [[wiki link]] (non-greedy, no nested brackets).
var wikiLinkRe = regexp.MustCompile(`\[\[([^\[\]]+)\]\]`)

// inlineCodeRe matches an inline `code span`, so [[...]] inside code isn't
// mistaken for a link (e.g. the literal `[[…]]` used as a placeholder in prose).
var inlineCodeRe = regexp.MustCompile("`[^`]*`")

// wikiLink is a single [[...]] occurrence in a note.
type wikiLink struct {
	Raw  string // target exactly as written; a slash in it changes its meaning
	Line int    // 1-based line number of the occurrence
	Text string // trimmed source line, for context
	// Start and End are Raw's byte offsets in the note, so a move can rewrite
	// exactly this occurrence and not the same text shown in a code span.
	Start, End int
}

// linkGraph is a snapshot of all wiki-links across the vault. Names live in
// vaultView instead, which can be built without reading anything.
type linkGraph struct {
	notes    []string              // all note paths (relative to root), sorted
	outgoing map[string][]wikiLink // note path -> links it contains
}

// linkTarget is the addressable part of a [[...]]: alias, heading and block
// suffixes stripped, whitespace trimmed. What remains is either a name or a
// path, and which one decides how it resolves (see resolveTarget).
func linkTarget(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "|#^"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// noteName normalizes a link target or filename to a comparison key: the last
// path component, without a .md extension, lowercased.
func noteName(s string) string {
	s = filepath.Base(linkTarget(s))
	s = strings.TrimSuffix(s, ".md")
	return strings.ToLower(s)
}

// hasParentSegment reports whether a target tries to walk upwards. Such a link
// does not resolve: it is the most move-brittle form there is, and the only one
// that could leave the vault. resolve() would catch an escape, but declining the
// input is cheaper than relying on that.
func hasParentSegment(target string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(target), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// vaultView is the vault as link resolution sees it: which files exist, and
// which notes carry each name. resolveTarget consults nothing else, so a move
// can be checked against the vault as it will be — a copy with one path swapped
// — before anything on disk changes. Every link question goes through it, which
// keeps a link meaning the same thing to /links, backlinks, orphans and a move.
//
// It is built from fd, not a walk, so it agrees with /find and find_files about
// what is in the vault: hidden paths are absent, and a symlinked folder is not
// descended into.
type vaultView struct {
	// files maps each path to the file it resolves to: itself, or for a symlink
	// its target — which is what a stat of the link reports.
	files  map[string]string
	byName map[string][]string // noteName -> regular .md files carrying it, sorted
}

func loadVault(ctx context.Context) (*vaultView, error) {
	regular, err := fdRun(ctx, []string{"--color=never", "--no-ignore", "--type", "f", "--search-path", "."})
	if err != nil {
		return nil, err
	}
	links, err := fdRun(ctx, []string{"--color=never", "--no-ignore", "--type", "l", "--search-path", "."})
	if err != nil {
		return nil, err
	}
	v := &vaultView{files: make(map[string]string, len(regular)+len(links))}
	for _, p := range regular {
		v.files[p] = p
	}
	// resolve follows the link and refuses a target outside the vault or on a
	// hidden path; a dangling link or one to a folder resolves to nothing.
	for _, p := range links {
		abs, err := resolve(p)
		if err != nil {
			continue
		}
		if info, err := os.Stat(abs); err == nil && !info.IsDir() {
			v.files[p] = relPath(abs)
		}
	}
	v.index()
	return v, nil
}

// index derives byName from files. Only regular files are named — a symlink is
// reachable by its path, not by a second name for the note it points at. The
// suffix test ignores case, as fd's '*.md' glob does.
func (v *vaultView) index() {
	v.byName = map[string][]string{}
	for p, target := range v.files {
		if p == target && strings.HasSuffix(strings.ToLower(p), ".md") {
			key := noteName(p)
			v.byName[key] = append(v.byName[key], p)
		}
	}
	for _, hits := range v.byName {
		sort.Strings(hits)
	}
}

// file returns the file an existing path resolves to, trying the name as given
// and then with .md appended, or "" if neither exists.
func (v *vaultView) file(rel string) string {
	rel = filepath.Clean(rel)
	for _, candidate := range []string{rel, rel + ".md"} {
		if p, ok := v.files[candidate]; ok {
			return p
		}
	}
	return ""
}

// notes lists the regular .md files, sorted: the notes whose links count.
func (v *vaultView) notes() []string {
	var out []string
	for _, hits := range v.byName {
		out = append(out, hits...)
	}
	sort.Strings(out)
	return out
}

// changed returns a copy with the removed paths gone and the added ones present
// as regular files. A symlink to a removed file goes too: on disk it would now
// dangle, and a dangling link resolves to nothing.
func (v *vaultView) changed(removed, added []string) *vaultView {
	gone := map[string]bool{}
	for _, p := range removed {
		gone[p] = true
	}
	w := &vaultView{files: make(map[string]string, len(v.files)+len(added))}
	for p, target := range v.files {
		if !gone[p] && !gone[target] {
			w.files[p] = target
		}
	}
	for _, p := range added {
		w.files[p] = p
	}
	w.index()
	return w
}

// linkResolution is what one [[...]] resolved to. Candidates is filled only when
// a name matched several notes: the link then resolves to nothing, and the
// ambiguity is reported rather than settled by guessing.
type linkResolution struct {
	Path       string
	Candidates []string
	Reason     string
}

// resolveTarget implements the link grammar documented in the README. `from` is
// the linking note's vault-relative path, and v the vault to resolve against.
//
// A slash decides everything: without one the target is a name, with one it is a
// path from the vault root.
func resolveTarget(raw, from string, v *vaultView) linkResolution {
	target := linkTarget(raw)
	if target == "" {
		return linkResolution{Reason: "empty link target"}
	}
	if hasParentSegment(target) {
		return linkResolution{Reason: "a '..' segment is not allowed in a link"}
	}
	dir := filepath.Dir(from)
	if dir == "." {
		dir = ""
	}

	switch {
	// Explicitly the linking note's own folder.
	case strings.HasPrefix(target, "./"):
		if p := v.file(filepath.Join(dir, strings.TrimPrefix(target, "./"))); p != "" {
			return linkResolution{Path: p}
		}
		return linkResolution{Reason: "no such note beside " + from}

	// A path: from the vault root, leading slash optional.
	case strings.Contains(target, "/"):
		if p := v.file(strings.TrimPrefix(target, "/")); p != "" {
			return linkResolution{Path: p}
		}
		return linkResolution{Reason: "no note at that path"}
	}

	// A name: the linking note's folder first, so a per-folder README convention
	// resolves to the README beside you.
	if dir != "" {
		if p := v.file(filepath.Join(dir, target)); p != "" {
			return linkResolution{Path: p}
		}
	}
	switch hits := v.byName[noteName(target)]; len(hits) {
	case 0:
		return linkResolution{Reason: "no note with that name"}
	case 1:
		return linkResolution{Path: hits[0]}
	default:
		return linkResolution{Candidates: hits, Reason: "several notes share that name"}
	}
}

// parseLinks extracts one note's [[...]] occurrences, keeping each target as
// written — resolution needs the raw form, since a slash in it changes what the
// link means. Fenced blocks and inline code spans are skipped: a [[...]] shown
// as an example is not a link.
func parseLinks(data []byte) []wikiLink {
	var out []wikiLink
	inFence := false
	offset := 0 // byte offset of the current line in data
	for i, line := range strings.Split(string(data), "\n") {
		lineStart := offset
		offset += len(line) + 1
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		// Blank code spans out with as many spaces as they held, so positions in
		// scan are positions in line.
		scan := inlineCodeRe.ReplaceAllStringFunc(line, func(s string) string { return strings.Repeat(" ", len(s)) })
		for _, m := range wikiLinkRe.FindAllStringSubmatchIndex(scan, -1) {
			out = append(out, wikiLink{
				Raw: line[m[2]:m[3]], Line: i + 1, Text: strings.TrimSpace(line),
				Start: lineStart + m[2], End: lineStart + m[3],
			})
		}
	}
	return out
}

// buildGraph reads every note and parses its wiki-links, for the questions that
// genuinely need a reverse index — backlinks, orphans and a move. Resolving one
// note's outgoing links costs a single read (see outgoingCore), where this costs
// a read of the whole vault. Rebuilt per call, which the vault's size affords
// and which avoids stale-cache concerns.
func buildGraph(v *vaultView) *linkGraph {
	notes := v.notes()
	g := &linkGraph{notes: notes, outgoing: map[string][]wikiLink{}}
	for _, rel := range notes {
		abs, err := resolve(rel)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue // an unreadable note is not a reason to fail the whole query
		}
		if links := parseLinks(data); len(links) > 0 {
			g.outgoing[rel] = links
		}
	}
	return g
}

// requireNote resolves a caller path and confirms it points at an existing file.
func requireNote(p string) (string, error) {
	path, err := resolve(p)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(path); err != nil {
		return "", err
	} else if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", p)
	}
	return path, nil
}

// ---- backlinks ----

type BacklinksInput struct {
	Path string `json:"path" jsonschema:"note (path relative to root) to find backlinks for"`
}

type Backlink struct {
	Path string `json:"path"` // note containing the link
	Line int    `json:"line"`
	Text string `json:"text"`
}

type BacklinksOutput struct {
	Path      string     `json:"path"`
	Backlinks []Backlink `json:"backlinks"`
}

func Backlinks(ctx context.Context, req *mcp.CallToolRequest, in BacklinksInput) (*mcp.CallToolResult, BacklinksOutput, error) {
	path, err := requireNote(in.Path)
	if err != nil {
		return nil, BacklinksOutput{}, err
	}
	v, err := loadVault(ctx)
	if err != nil {
		return nil, BacklinksOutput{}, err
	}
	g := buildGraph(v)
	self := relPath(path)

	// A backlink is a link that *resolves to* this note — not one whose name
	// happens to match. Under the path-aware grammar those differ: a link can
	// name this basename and resolve elsewhere, or carry a path and land here.
	out := BacklinksOutput{Path: self}
	for _, note := range g.notes {
		if note == self {
			continue // ignore self-links
		}
		for _, l := range g.outgoing[note] {
			if resolveTarget(l.Raw, note, v).Path == self {
				out.Backlinks = append(out.Backlinks, Backlink{Path: note, Line: l.Line, Text: l.Text})
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d backlink(s) to %s:\n", len(out.Backlinks), self)
	for _, bl := range out.Backlinks {
		fmt.Fprintf(&b, "%s:%d: %s\n", bl.Path, bl.Line, bl.Text)
	}
	return textResult("%s", b.String()), out, nil
}

// ---- outgoing_links ----

type OutgoingLinksInput struct {
	Path string `json:"path" jsonschema:"note (path relative to root) whose outgoing links to list"`
}

type OutLink struct {
	Target   string `json:"target"`   // target as written, minus any |alias or #heading
	Resolved string `json:"resolved"` // note path it resolves to, or "" if broken
	Broken   bool   `json:"broken"`
	// Reason says why a broken link didn't resolve, and Candidates lists the
	// notes an ambiguous name matched — an ambiguity is reported rather than
	// settled by picking one, since with a per-folder README convention the
	// guess would usually be wrong.
	Reason     string   `json:"reason,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
	Line       int      `json:"line"`
}

type OutgoingLinksOutput struct {
	Path  string    `json:"path"`
	Links []OutLink `json:"links"`
}

// outgoingCore resolves one note's links: a single read, and one fd listing if
// the note has any links. No graph, and no other note is read. Shared by the
// tool and GET /links.
func outgoingCore(ctx context.Context, notePath string) (OutgoingLinksOutput, error) {
	abs, err := requireNote(notePath)
	if err != nil {
		return OutgoingLinksOutput{}, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return OutgoingLinksOutput{}, err
	}
	self := relPath(abs)
	out := OutgoingLinksOutput{Path: self}

	links := parseLinks(data)
	if len(links) == 0 {
		return out, nil
	}
	v, err := loadVault(ctx)
	if err != nil {
		return OutgoingLinksOutput{}, err
	}
	for _, l := range links {
		r := resolveTarget(l.Raw, self, v)
		out.Links = append(out.Links, OutLink{
			Target:     linkTarget(l.Raw),
			Resolved:   r.Path,
			Broken:     r.Path == "",
			Reason:     r.Reason,
			Candidates: r.Candidates,
			Line:       l.Line,
		})
	}
	return out, nil
}

func OutgoingLinks(ctx context.Context, req *mcp.CallToolRequest, in OutgoingLinksInput) (*mcp.CallToolResult, OutgoingLinksOutput, error) {
	out, err := outgoingCore(ctx, in.Path)
	if err != nil {
		return nil, OutgoingLinksOutput{}, err
	}
	self := out.Path

	var b strings.Builder
	fmt.Fprintf(&b, "%s links to %d target(s):\n", self, len(out.Links))
	for _, l := range out.Links {
		if l.Broken {
			fmt.Fprintf(&b, "[[%s]] -> (broken) (line %d)\n", l.Target, l.Line)
		} else {
			fmt.Fprintf(&b, "[[%s]] -> %s (line %d)\n", l.Target, l.Resolved, l.Line)
		}
	}
	return textResult("%s", b.String()), out, nil
}

// ---- orphans ----

type OrphansInput struct{}

type OrphansOutput struct {
	Orphans []string `json:"orphans"`
}

func Orphans(ctx context.Context, req *mcp.CallToolRequest, in OrphansInput) (*mcp.CallToolResult, OrphansOutput, error) {
	v, err := loadVault(ctx)
	if err != nil {
		return nil, OrphansOutput{}, err
	}
	g := buildGraph(v)

	// Resolve through the same rule the other two use, so "linked" here means
	// exactly what a backlink means there.
	linked := map[string]bool{}
	for _, note := range g.notes {
		for _, l := range g.outgoing[note] {
			if p := resolveTarget(l.Raw, note, v).Path; p != "" && p != note {
				linked[p] = true
			}
		}
	}

	var out OrphansOutput
	for _, note := range g.notes {
		if !linked[note] {
			out.Orphans = append(out.Orphans, note)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d orphan note(s) (nothing links to them):\n", len(out.Orphans))
	for _, o := range out.Orphans {
		fmt.Fprintf(&b, "%s\n", o)
	}
	return textResult("%s", b.String()), out, nil
}
