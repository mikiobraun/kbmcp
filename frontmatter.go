package main

// Frontmatter reading: return the raw YAML block at the head of a note,
// uninterpreted. Deliberately not parsed — the calling agent reads YAML fine,
// and the raw block is the only view that shows structure (that `attachments`
// is a list of maps, say) which any field-level projection flattens away. It
// also keeps this tool free of a YAML dependency, and lets a malformed block be
// returned as-is rather than becoming an error.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultFrontmatterCap bounds how much of one file's frontmatter is returned.
// It is a caller-supplied budget, not a guess about how big frontmatter "should"
// be: passing a bigger cap always gets more.
const defaultFrontmatterCap = 2000

// maxFrontmatterPaths bounds how many files one call may ask for, mirroring the
// page cap the listing tools use.
const maxFrontmatterPaths = maxListMax

type ReadFrontmatterInput struct {
	Paths []string `json:"paths" jsonschema:"files to read, relative to the served root; the output has one entry per path, in this order"`
	Cap   int      `json:"cap,omitempty" jsonschema:"maximum bytes of frontmatter returned per file (default 2000); if the block is longer, or never closed, what fits is returned with truncated=true"`
}

type FrontmatterEntry struct {
	Path string `json:"path"`
	// HasFrontmatter is true when the file starts with a '---' delimiter line,
	// even if the block between delimiters is empty.
	HasFrontmatter bool   `json:"has_frontmatter"`
	Frontmatter    string `json:"frontmatter"`
	Truncated      bool   `json:"truncated"`
	// Error is set when the file could not be read at all (missing, a directory,
	// binary). It keeps a mistyped path distinguishable from a file that
	// genuinely has no frontmatter, which otherwise look identical.
	Error string `json:"error,omitempty"`
}

type ReadFrontmatterOutput struct {
	Entries []FrontmatterEntry `json:"entries"`
}

// fmDelim is the frontmatter delimiter line. Only "---" is recognised; YAML's
// "..." document end is legal but unseen in practice, and a file using it reads
// as unterminated rather than being silently accepted.
const fmDelim = "---"

// cutFrontmatter extracts the raw frontmatter block from a file's leading bytes.
//
// The rule is fully determined: the file must open with a "---" delimiter line
// at byte 0; the block ends at the first later line that is exactly "---"; what
// lies between is returned verbatim, delimiters excluded. Scanning stops after
// limit bytes — whether the block is simply longer than the budget or the file
// never closes it, the result is the same truncated prefix, so there is no
// separate "malformed" state to reason about.
func cutFrontmatter(data []byte, limit int) (block string, has, truncated bool) {
	rest, ok := trimDelimLine(data)
	if !ok {
		return "", false, false
	}
	// Walk lines until the closing delimiter or until the block would exceed the
	// budget. The bound is the block offset, not the bytes scanned: a delimiter
	// starting at exactly limit closes a block of exactly limit bytes, which
	// fits and is complete.
	for off := 0; off <= limit && off < len(rest); {
		line, next := lineAt(rest, off)
		if strings.TrimSuffix(line, "\r") == fmDelim {
			return string(rest[:off]), true, false
		}
		if next == off {
			break
		}
		off = next
	}
	return truncateUTF8(string(rest), limit), true, true
}

// trimDelimLine reports whether data opens with a "---" delimiter line and
// returns everything after it.
func trimDelimLine(data []byte) ([]byte, bool) {
	line, next := lineAt(data, 0)
	if strings.TrimSuffix(line, "\r") != fmDelim {
		return nil, false
	}
	return data[next:], true
}

// lineAt returns the line starting at off (without its newline) and the offset
// of the next line. At the end of input, next equals len(data).
func lineAt(data []byte, off int) (line string, next int) {
	i := bytes.IndexByte(data[off:], '\n')
	if i < 0 {
		return string(data[off:]), len(data)
	}
	return string(data[off : off+i]), off + i + 1
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune. It does not
// back off to a line boundary: a partial last line is honest, and the caller is
// told the value was truncated.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// errText renders an error without the absolute path a *fs.PathError carries:
// every other tool reports paths relative to the served root, and the root's
// location is not the caller's business.
func errText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// readOneFrontmatter reads one already-resolved file's frontmatter. Per-file
// problems are reported in the entry rather than failing the batch, so one bad
// path does not cost the caller the other results.
func readOneFrontmatter(rel, abs string, limit int) FrontmatterEntry {
	e := FrontmatterEntry{Path: rel}
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
	// Only the head of the file is needed: the opening delimiter line, a
	// cap-sized block, and the closing delimiter line. Reading that much means a
	// huge note is never slurped whole, while a block that ends exactly at the
	// cap still has its closing delimiter inside the buffer.
	f, err := os.Open(abs)
	if err != nil {
		e.Error = "cannot read: " + errText(err)
		return e
	}
	defer f.Close()
	buf := make([]byte, limit+2*(len(fmDelim)+2))
	// ReadFull, not Read: a short read on a regular file is legal and would
	// silently look like a truncated block.
	n, err := io.ReadFull(f, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		err = nil
	}
	if n == 0 && err != nil {
		// An empty file simply has no frontmatter; anything else is an error.
		if info.Size() == 0 {
			return e
		}
		e.Error = "cannot read: " + errText(err)
		return e
	}
	data := buf[:n]
	if looksBinary(data[:min(len(data), 512)]) {
		e.Error = "appears to be a binary file"
		return e
	}
	e.Frontmatter, e.HasFrontmatter, e.Truncated = cutFrontmatter(data, limit)
	return e
}

func ReadFrontmatter(ctx context.Context, req *mcp.CallToolRequest, in ReadFrontmatterInput) (*mcp.CallToolResult, ReadFrontmatterOutput, error) {
	if len(in.Paths) == 0 {
		return nil, ReadFrontmatterOutput{}, fmt.Errorf("paths must not be empty")
	}
	if len(in.Paths) > maxFrontmatterPaths {
		return nil, ReadFrontmatterOutput{}, fmt.Errorf("too many paths: %d (max %d); split the call", len(in.Paths), maxFrontmatterPaths)
	}
	limit := in.Cap
	if limit <= 0 {
		limit = defaultFrontmatterCap
	}
	if limit > maxFileBytes {
		limit = maxFileBytes
	}

	// Resolve every path up front and fail the whole call on an escape: unlike a
	// missing file, that means the caller asked for something it may not have.
	abs := make([]string, len(in.Paths))
	for i, p := range in.Paths {
		a, err := resolve(p)
		if err != nil {
			return nil, ReadFrontmatterOutput{}, err
		}
		abs[i] = a
	}

	out := ReadFrontmatterOutput{Entries: make([]FrontmatterEntry, 0, len(in.Paths))}
	var b strings.Builder
	for i, p := range in.Paths {
		e := readOneFrontmatter(p, abs[i], limit)
		out.Entries = append(out.Entries, e)

		b.WriteString(e.Path)
		b.WriteByte('\n')
		switch {
		case e.Error != "":
			fmt.Fprintf(&b, "(error: %s)\n", e.Error)
		case !e.HasFrontmatter:
			b.WriteString("(no frontmatter)\n")
		default:
			// Wrap in delimiters so block boundaries are unambiguous in the text
			// view, even though the structured field excludes them.
			b.WriteString(fmDelim + "\n")
			b.WriteString(e.Frontmatter)
			if !strings.HasSuffix(e.Frontmatter, "\n") {
				b.WriteByte('\n')
			}
			if e.Truncated {
				fmt.Fprintf(&b, "... (truncated at %d bytes)\n", limit)
			}
			b.WriteString(fmDelim + "\n")
		}
		b.WriteByte('\n')
	}
	return textResult("%s", b.String()), out, nil
}
