package main

// read_outline: a note's headings, each with the line range its section spans,
// so an agent can see how a long note is organised and then read one section
// with read_lines instead of pulling the whole file. The ranges are the point: a
// bare list of headings would still leave the agent reading everything to find
// where a section ends.
//
// A separate tool rather than a flag on read_file, for the reason the search
// tools name their modes in their fields: what a call returns should not hinge
// on a flag the caller read once in the tool list and has since forgotten. This
// returns headings, read_file returns content.

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxOutlineLevel is the deepest heading listed. A deeper heading is not a
// boundary either, so its text simply belongs to the section around it and the
// ranges still cover the note.
const maxOutlineLevel = 3

// maxOutlinePaths bounds how many files one call may ask for, mirroring
// read_frontmatter.
const maxOutlinePaths = maxListMax

// atxHeadingRe matches a '#' heading as CommonMark defines it: up to three
// spaces of indent (four would be code), one to six '#', then whitespace or the
// end of the line. The whitespace is what keeps an Obsidian #tag at the start of
// a line from reading as a heading.
var atxHeadingRe = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?[ \t]*$`)

type ReadOutlineInput struct {
	Paths []string `json:"paths" jsonschema:"notes to outline, relative to the served root; the output has one entry per path, in this order"`
}

type Heading struct {
	Level int    `json:"level"` // 1 to 3: the number of '#'
	Text  string `json:"text"`
	Line  int    `json:"line"` // the heading's own line, 1-based as in read_lines
	// EndLine is the last line of the section: the line before the next heading
	// of the same or a higher level, or the file's last line. Subsections are
	// inside it.
	EndLine int `json:"end_line"`
}

type OutlineEntry struct {
	Path string `json:"path"`
	// Lines is the file's line count, so what comes before the first heading is
	// lines 1 to Headings[0].Line-1.
	Lines    int       `json:"lines"`
	Headings []Heading `json:"headings"`
	// Error is set when the file could not be read at all (missing, a directory,
	// binary), which keeps a mistyped path apart from a note without headings.
	Error string `json:"error,omitempty"`
}

type ReadOutlineOutput struct {
	Entries []OutlineEntry `json:"entries"`
}

// outline finds the headings in a note's text and the range each one spans.
//
// Lines are counted as read_lines counts them, so a range can be passed straight
// to it: a final newline does not start another line. A frontmatter block is
// skipped, since a YAML comment starts with '#', and so are fenced code blocks,
// by the same rule parseLinks uses. Frontmatter here is only a block that is
// closed: an opening '---' with no closing one is a horizontal rule, not a
// header that swallows the note.
func outline(text string) (lines int, headings []Heading) {
	all := strings.Split(text, "\n")
	if all[len(all)-1] == "" {
		all = all[:len(all)-1]
	}

	start := 0
	if len(all) > 0 && strings.TrimSuffix(all[0], "\r") == fmDelim {
		for i := 1; i < len(all); i++ {
			if strings.TrimSuffix(all[i], "\r") == fmDelim {
				start = i + 1
				break
			}
		}
	}

	inFence := false
	for i := start; i < len(all); i++ {
		line := strings.TrimSuffix(all[i], "\r")
		if isFenceLine(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		m := atxHeadingRe.FindStringSubmatch(line)
		if m == nil || len(m[1]) > maxOutlineLevel {
			continue
		}
		headings = append(headings, Heading{Level: len(m[1]), Text: headingText(m[2]), Line: i + 1})
	}

	for i := range headings {
		headings[i].EndLine = len(all)
		for _, next := range headings[i+1:] {
			if next.Level <= headings[i].Level {
				headings[i].EndLine = next.Line - 1
				break
			}
		}
	}
	return len(all), headings
}

// headingText drops an optional closing run of '#'. It counts as closing only
// when whitespace separates it from the text, so "C#" keeps its '#'.
func headingText(s string) string {
	s = strings.TrimSpace(s)
	if t := strings.TrimRight(s, "#"); t == "" || strings.HasSuffix(t, " ") || strings.HasSuffix(t, "\t") {
		s = strings.TrimSpace(t)
	}
	return s
}

// readOneOutline outlines one already-resolved file. Per-file problems are
// reported in the entry rather than failing the batch.
func readOneOutline(rel, abs string) OutlineEntry {
	e := OutlineEntry{Path: rel}
	info, err := os.Stat(abs)
	switch {
	case os.IsNotExist(err):
		e.Error = "no such file"
		return e
	case err != nil:
		e.Error = "cannot read: " + errText(err)
		return e
	case info.IsDir():
		e.Error = "is a directory"
		return e
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		e.Error = "cannot read: " + errText(err)
		return e
	}
	if looksBinary(data[:min(len(data), 512)]) {
		e.Error = "appears to be a binary file"
		return e
	}
	e.Lines, e.Headings = outline(string(data))
	return e
}

func ReadOutline(ctx context.Context, req *mcp.CallToolRequest, in ReadOutlineInput) (*mcp.CallToolResult, ReadOutlineOutput, error) {
	if len(in.Paths) == 0 {
		return nil, ReadOutlineOutput{}, fmt.Errorf("paths must not be empty")
	}
	if len(in.Paths) > maxOutlinePaths {
		return nil, ReadOutlineOutput{}, fmt.Errorf("too many paths: %d (max %d); split the call", len(in.Paths), maxOutlinePaths)
	}

	// Resolve every path up front and fail the whole call on an escape: unlike a
	// missing file, that means the caller asked for something it may not have.
	abs := make([]string, len(in.Paths))
	for i, p := range in.Paths {
		a, err := resolve(p)
		if err != nil {
			return nil, ReadOutlineOutput{}, err
		}
		abs[i] = a
	}

	out := ReadOutlineOutput{Entries: make([]OutlineEntry, 0, len(in.Paths))}
	var b strings.Builder
	for i, p := range in.Paths {
		e := readOneOutline(p, abs[i])
		// A JSON null would make "no headings" awkward for every caller.
		if e.Headings == nil {
			e.Headings = []Heading{}
		}
		out.Entries = append(out.Entries, e)

		switch {
		case e.Error != "":
			fmt.Fprintf(&b, "%s\n(error: %s)\n", e.Path, e.Error)
		case len(e.Headings) == 0:
			fmt.Fprintf(&b, "%s (%d lines)\n(no headings)\n", e.Path, e.Lines)
		default:
			fmt.Fprintf(&b, "%s (%d lines)\n", e.Path, e.Lines)
			for _, h := range e.Headings {
				fmt.Fprintf(&b, "%s%s %s (lines %d-%d)\n",
					strings.Repeat("  ", h.Level-1), strings.Repeat("#", h.Level), h.Text, h.Line, h.EndLine)
			}
		}
		b.WriteByte('\n')
	}
	return textResult("%s", b.String()), out, nil
}
