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
}

// linkGraph is a snapshot of all wiki-links across the vault. Names live in
// noteIndex instead, which can be built without reading anything.
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

// statNote returns the vault-relative path of an existing note at rel, trying
// the name as given and then with .md appended. A directory is not a note, and
// resolve() keeps the lookup inside the vault.
func statNote(rel string) string {
	for _, candidate := range []string{rel, rel + ".md"} {
		abs, err := resolve(candidate)
		if err != nil {
			continue
		}
		if info, err := os.Stat(abs); err == nil && !info.IsDir() {
			return relPath(abs)
		}
	}
	return ""
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
// the linking note's vault-relative path; `idx` is consulted only for the
// vault-wide name lookup and may be nil to skip it — the caller then learns from
// the reason that it needs an index and can ask again.
//
// A slash decides everything: without one the target is a name, with one it is a
// path from the vault root.
func resolveTarget(raw, from string, idx *noteIndex) linkResolution {
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
		if p := statNote(filepath.Join(dir, strings.TrimPrefix(target, "./"))); p != "" {
			return linkResolution{Path: p}
		}
		return linkResolution{Reason: "no such note beside " + from}

	// A path: from the vault root, leading slash optional.
	case strings.Contains(target, "/"):
		if p := statNote(strings.TrimPrefix(target, "/")); p != "" {
			return linkResolution{Path: p}
		}
		return linkResolution{Reason: "no note at that path"}
	}

	// A name: the linking note's folder first, so a per-folder README convention
	// resolves to the README beside you.
	if dir != "" {
		if p := statNote(filepath.Join(dir, target)); p != "" {
			return linkResolution{Path: p}
		}
	}
	if idx == nil {
		return linkResolution{Reason: "not a sibling; needs the name index"}
	}
	switch hits := idx.byName[noteName(target)]; len(hits) {
	case 0:
		return linkResolution{Reason: "no note with that name"}
	case 1:
		return linkResolution{Path: hits[0]}
	default:
		return linkResolution{Candidates: hits, Reason: "several notes share that name"}
	}
}

// noteIndex maps a note name to every note carrying it. It is built from the
// same fd-backed listing that /find and find_files use, so link resolution
// agrees with them about what is in the vault instead of walking with a third
// set of rules. No note is read: resolution needs names, not contents.
type noteIndex struct {
	byName map[string][]string
}

func buildIndex(ctx context.Context) (*noteIndex, error) {
	paths, err := fdPaths(ctx, FindInput{Glob: "*.md"})
	if err != nil {
		return nil, err
	}
	idx := &noteIndex{byName: map[string][]string{}}
	for _, p := range paths {
		key := noteName(filepath.Base(p))
		idx.byName[key] = append(idx.byName[key], p)
	}
	return idx, nil
}

// parseLinks extracts one note's [[...]] occurrences, keeping each target as
// written — resolution needs the raw form, since a slash in it changes what the
// link means. Fenced blocks and inline code spans are skipped: a [[...]] shown
// as an example is not a link.
func parseLinks(data []byte) []wikiLink {
	var out []wikiLink
	inFence := false
	for i, line := range strings.Split(string(data), "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		scan := inlineCodeRe.ReplaceAllString(line, " ")
		for _, m := range wikiLinkRe.FindAllStringSubmatch(scan, -1) {
			out = append(out, wikiLink{Raw: m[1], Line: i + 1, Text: strings.TrimSpace(line)})
		}
	}
	return out
}

// buildGraph reads every note and parses its wiki-links, for the questions that
// genuinely need a reverse index — backlinks and orphans. Nothing else should
// call it: resolving one note's outgoing links costs a read and some stats (see
// outgoingCore), where this costs a read of the whole vault. Rebuilt per call,
// which the vault's size affords and which avoids stale-cache concerns.
func buildGraph(ctx context.Context) (*linkGraph, error) {
	notes, err := fdPaths(ctx, FindInput{Glob: "*.md"})
	if err != nil {
		return nil, err
	}
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
	sort.Strings(g.notes)
	return g, nil
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
	g, err := buildGraph(ctx)
	if err != nil {
		return nil, BacklinksOutput{}, err
	}
	idx, err := buildIndex(ctx)
	if err != nil {
		return nil, BacklinksOutput{}, err
	}
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
			if resolveTarget(l.Raw, note, idx).Path == self {
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

// outgoingCore resolves one note's links: a single read, a stat per link, and
// at most one fd run — only if some bare name isn't a sibling. No graph, and no
// other note is read. Shared by the tool and GET /links.
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

	// First pass without an index, so the vault listing is only paid for when a
	// link actually needs a name lookup.
	var idx *noteIndex
	links := parseLinks(data)
	for _, l := range links {
		r := resolveTarget(l.Raw, self, idx)
		if r.Path == "" && idx == nil && r.Reason == "not a sibling; needs the name index" {
			if idx, err = buildIndex(ctx); err != nil {
				return OutgoingLinksOutput{}, err
			}
			r = resolveTarget(l.Raw, self, idx)
		}
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
	g, err := buildGraph(ctx)
	if err != nil {
		return nil, OrphansOutput{}, err
	}
	idx, err := buildIndex(ctx)
	if err != nil {
		return nil, OrphansOutput{}, err
	}

	// Resolve through the same rule the other two use, so "linked" here means
	// exactly what a backlink means there.
	linked := map[string]bool{}
	for _, note := range g.notes {
		for _, l := range g.outgoing[note] {
			if p := resolveTarget(l.Raw, note, idx).Path; p != "" && p != note {
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
