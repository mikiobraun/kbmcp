package main

import (
	"context"
	"fmt"
	"io/fs"
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
	Target string // normalized target note name (see noteName)
	Line   int    // 1-based line number of the occurrence
	Text   string // trimmed source line, for context
}

// linkGraph is a snapshot of all wiki-links across the vault.
type linkGraph struct {
	notes    []string              // all note paths (relative to root), sorted
	byName   map[string]string     // normalized note name -> relative path (first match wins)
	outgoing map[string][]wikiLink // note path -> links it contains
}

// noteName normalizes a link target or filename to a comparison key: the last
// path component, without a .md extension, lowercased, with any |alias or
// #heading suffix removed.
func noteName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "|#"); i >= 0 {
		s = s[:i]
	}
	s = filepath.Base(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".md")
	return strings.ToLower(s)
}

// buildGraph walks the vault and parses every .md file's wiki-links. It is
// rebuilt per call: the vault is small and this avoids any stale-cache concerns.
func buildGraph() (*linkGraph, error) {
	g := &linkGraph{byName: map[string]string{}, outgoing: map[string][]wikiLink{}}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries rather than aborting
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(p), ".md") {
			return nil
		}
		rel := relPath(p)
		g.notes = append(g.notes, rel)
		if name := noteName(filepath.Base(p)); g.byName[name] == "" {
			g.byName[name] = rel
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		inFence := false
		for i, line := range strings.Split(string(data), "\n") {
			// Skip fenced code blocks; links inside them aren't real links.
			if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			// Match against a copy with inline-code spans removed.
			scan := inlineCodeRe.ReplaceAllString(line, " ")
			for _, m := range wikiLinkRe.FindAllStringSubmatch(scan, -1) {
				g.outgoing[rel] = append(g.outgoing[rel], wikiLink{
					Target: noteName(m[1]),
					Line:   i + 1,
					Text:   strings.TrimSpace(line),
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
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
	g, err := buildGraph()
	if err != nil {
		return nil, BacklinksOutput{}, err
	}
	target := noteName(filepath.Base(path))
	self := relPath(path)

	out := BacklinksOutput{Path: self}
	for _, note := range g.notes {
		if note == self {
			continue // ignore self-links
		}
		for _, l := range g.outgoing[note] {
			if l.Target == target {
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
	Target   string `json:"target"`   // normalized link target name
	Resolved string `json:"resolved"` // note path it resolves to, or "" if broken
	Broken   bool   `json:"broken"`
	Line     int    `json:"line"`
}

type OutgoingLinksOutput struct {
	Path  string    `json:"path"`
	Links []OutLink `json:"links"`
}

func OutgoingLinks(ctx context.Context, req *mcp.CallToolRequest, in OutgoingLinksInput) (*mcp.CallToolResult, OutgoingLinksOutput, error) {
	path, err := requireNote(in.Path)
	if err != nil {
		return nil, OutgoingLinksOutput{}, err
	}
	g, err := buildGraph()
	if err != nil {
		return nil, OutgoingLinksOutput{}, err
	}
	self := relPath(path)

	out := OutgoingLinksOutput{Path: self}
	for _, l := range g.outgoing[self] {
		resolved := g.byName[l.Target]
		out.Links = append(out.Links, OutLink{
			Target:   l.Target,
			Resolved: resolved,
			Broken:   resolved == "",
			Line:     l.Line,
		})
	}

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
	g, err := buildGraph()
	if err != nil {
		return nil, OrphansOutput{}, err
	}

	linked := map[string]bool{}
	for _, note := range g.notes {
		for _, l := range g.outgoing[note] {
			if p := g.byName[l.Target]; p != "" && p != note {
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
