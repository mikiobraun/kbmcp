package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- batch_edits ----

// BatchOp is one operation in a batch: creating/overwriting a whole file
// ("write"), replacing a substring in an existing one ("edit"), removing one
// ("delete"), or moving one ("move").
type BatchOp struct {
	Op         string `json:"op" jsonschema:"'write' to create or overwrite a file with content, 'edit' to replace old_string with new_string, 'delete' to remove a file (only if its current content is committed; a symlink is removed itself), or 'move' to move a file to 'to' (same rules as move_file)"`
	Path       string `json:"path" jsonschema:"file to act on, relative to root"`
	To         string `json:"to,omitempty" jsonschema:"for op=move: the path to move the file to, relative to root; nothing may exist there yet"`
	Content    string `json:"content,omitempty" jsonschema:"for op=write: full new contents of the file"`
	OldString  string `json:"old_string,omitempty" jsonschema:"for op=edit: exact text to replace; must occur exactly once unless replace_all is set"`
	NewString  string `json:"new_string,omitempty" jsonschema:"for op=edit: text to replace it with"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"for op=edit: replace every occurrence instead of requiring a unique match"`
}

type BatchEditsInput struct {
	Message     string    `json:"message" jsonschema:"commit message for the whole batch (required)"`
	Ops         []BatchOp `json:"ops" jsonschema:"ordered list of write/edit/delete/move operations, applied in order and committed together as one commit"`
	AuthorName  string    `json:"author_name,omitempty" jsonschema:"name to attribute the commit to; defaults to the part of author_email before the @"`
	AuthorEmail string    `json:"author_email" jsonschema:"email to attribute the commit to (required): who or what is making this change, e.g. the agent's own address. Every commit names an author, so an automated writer stays distinguishable from a person"`
	DryRun      bool      `json:"dry_run,omitempty" jsonschema:"if true, validate and return the diffs without writing or committing anything"`
}

type BatchFileResult struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
	Deleted bool   `json:"deleted"`
	// RenamedFrom is where a moved file was, and Diff is then taken against its
	// content there, so a move shows only the links it rewrote.
	RenamedFrom string `json:"renamed_from,omitempty"`
	Diff        string `json:"diff"`
}

type BatchEditsOutput struct {
	Message   string            `json:"message"`
	DryRun    bool              `json:"dry_run"`
	Committed bool              `json:"committed"`
	Files     []BatchFileResult `json:"files"`
	Links     []LinkRewrite     `json:"links,omitempty"` // rewritten by move ops, in op order
}

// batchFile is one file's working state while a batch is validated.
type batchFile struct {
	orig    string // diff base: on-disk content before the batch, or movedFrom's
	cur     string // content after the ops so far
	existed bool   // on disk before the batch
	exists  bool   // present after the ops so far
	// movedFrom is the absolute path this file's content was on disk at before
	// the batch, when a move brought it here.
	movedFrom string
}

// batchState is a batch's evolving picture of the files it touches. Files are
// keyed by resolved absolute path, so two spellings of one file share one
// state; order records first-touch order for stable output.
type batchState struct {
	files map[string]*batchFile
	order []string
	links []LinkRewrite
}

func (s *batchState) track(p string, f *batchFile) {
	s.files[p] = f
	s.order = append(s.order, p)
}

// load returns the state of the file a write or edit targets.
func (s *batchState) load(rel string) (*batchFile, error) {
	p, err := resolve(rel)
	if err != nil {
		return nil, err
	}
	if f, seen := s.files[p]; seen {
		return f, nil
	}
	info, statErr := os.Stat(p)
	if statErr != nil {
		// Treat a missing file as empty/new; any other stat error is real.
		if os.IsNotExist(statErr) {
			f := &batchFile{}
			s.track(p, f)
			return f, nil
		}
		return nil, statErr
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", rel)
	}
	data, rdErr := os.ReadFile(p)
	if rdErr != nil {
		return nil, rdErr
	}
	if looksBinary(data[:min(len(data), 512)]) {
		return nil, fmt.Errorf("%s appears to be a binary file", rel)
	}
	f := &batchFile{orig: string(data), cur: string(data), existed: true, exists: true}
	s.track(p, f)
	return f, nil
}

// planBatch validates and applies every op in memory, against the state the
// ops before it left behind. Nothing on disk changes, so a single bad op aborts
// the whole batch having written nothing.
func planBatch(ctx context.Context, ops []BatchOp) (*batchState, error) {
	s := newBatchState()
	for i, op := range ops {
		switch op.Op {
		case "write":
			f, err := s.load(op.Path)
			if err != nil {
				return nil, fmt.Errorf("op %d (write %s): %w", i, op.Path, err)
			}
			f.cur = op.Content
			f.exists = true
		case "edit":
			if op.OldString == "" {
				return nil, fmt.Errorf("op %d (edit %s): old_string must not be empty", i, op.Path)
			}
			if op.OldString == op.NewString {
				return nil, fmt.Errorf("op %d (edit %s): old_string and new_string are identical", i, op.Path)
			}
			f, err := s.load(op.Path)
			if err != nil {
				return nil, fmt.Errorf("op %d (edit %s): %w", i, op.Path, err)
			}
			if !f.exists {
				return nil, fmt.Errorf("op %d (edit %s): no such file", i, op.Path)
			}
			count := strings.Count(f.cur, op.OldString)
			switch {
			case count == 0:
				return nil, fmt.Errorf("op %d (edit %s): old_string not found", i, op.Path)
			case count > 1 && !op.ReplaceAll:
				return nil, fmt.Errorf("op %d (edit %s): old_string occurs %d times; add context to make it unique, or set replace_all", i, op.Path, count)
			}
			if op.ReplaceAll {
				f.cur = strings.ReplaceAll(f.cur, op.OldString, op.NewString)
			} else {
				f.cur = strings.Replace(f.cur, op.OldString, op.NewString, 1)
			}
		case "delete":
			p, err := resolveLeaf(op.Path)
			if err != nil {
				return nil, fmt.Errorf("op %d (delete %s): %w", i, op.Path, err)
			}
			f, seen := s.files[p]
			switch {
			case !seen:
				old, err := prepareDelete(p)
				if err != nil {
					return nil, fmt.Errorf("op %d (delete %s): %w", i, op.Path, err)
				}
				f = &batchFile{orig: old, existed: true}
				s.track(p, f)
			case !f.exists:
				return nil, fmt.Errorf("op %d (delete %s): no such file", i, op.Path)
			case f.existed:
				// Earlier ops only changed it in memory; what the deletion would
				// discard is still what is on disk, so that is what must be
				// committed.
				if err := requireCommitted(p); err != nil {
					return nil, fmt.Errorf("op %d (delete %s): %w", i, op.Path, err)
				}
			}
			f.cur = ""
			f.exists = false
		case "move":
			if err := s.move(ctx, op.Path, op.To); err != nil {
				return nil, fmt.Errorf("op %d (move %s to %s): %w", i, op.Path, op.To, err)
			}
		default:
			return nil, fmt.Errorf("op %d: unknown op %q (want 'write', 'edit', 'delete', or 'move')", i, op.Op)
		}
	}
	return s, nil
}

// output describes what committing the batch would change.
func (s *batchState) output(message string, dryRun bool) BatchEditsOutput {
	// A move's source is reported on its destination, as a rename, rather than
	// as a deletion beside a creation.
	movedAway := map[string]bool{}
	for _, f := range s.files {
		if f.exists && f.movedFrom != "" {
			movedAway[f.movedFrom] = true
		}
	}
	out := BatchEditsOutput{Message: message, DryRun: dryRun, Links: s.links}
	for _, p := range s.order {
		f := s.files[p]
		if !f.existed && !f.exists {
			continue // created and deleted within the batch: nothing changes on disk
		}
		if !f.exists && movedAway[p] {
			continue
		}
		r := BatchFileResult{
			Path:    relPath(p),
			Created: !f.existed && f.movedFrom == "",
			Deleted: !f.exists,
			Diff:    diffText(f.orig, f.cur, 3),
		}
		if f.exists && f.movedFrom != "" {
			r.RenamedFrom = relPath(f.movedFrom)
		}
		out.Files = append(out.Files, r)
	}
	return out
}

// commit writes or removes every touched file, then commits them all in one
// commit. A move needs nothing of its own here: its source is removed and its
// destination written, and git pairs the two up as a rename.
func (s *batchState) commit(message, authorName, authorEmail string) (bool, error) {
	var paths []string
	for _, p := range s.order {
		f := s.files[p]
		switch {
		case f.exists:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return false, err
			}
			if err := os.WriteFile(p, []byte(f.cur), 0o644); err != nil {
				return false, err
			}
		case f.existed:
			if err := os.Remove(p); err != nil {
				return false, err
			}
			pruneEmptyDirs(filepath.Dir(p))
		default:
			continue
		}
		paths = append(paths, p)
	}
	return gitCommit(paths, message, authorName, authorEmail)
}

// BatchEdits applies a mix of write, edit, delete, and move operations
// atomically: every op is validated against the evolving in-memory state first,
// so a single bad op aborts the whole batch without touching disk. Only if all
// ops are valid are the files written or removed and committed together in one
// commit.
func BatchEdits(ctx context.Context, req *mcp.CallToolRequest, in BatchEditsInput) (*mcp.CallToolResult, BatchEditsOutput, error) {
	if strings.TrimSpace(in.Message) == "" {
		return nil, BatchEditsOutput{}, fmt.Errorf("message is required")
	}
	if len(in.Ops) == 0 {
		return nil, BatchEditsOutput{}, fmt.Errorf("ops must not be empty")
	}

	s, err := planBatch(ctx, in.Ops)
	if err != nil {
		return nil, BatchEditsOutput{}, err
	}
	out := s.output(in.Message, in.DryRun)
	if in.DryRun {
		return textResult("%s", renderBatch(out, "dry run, nothing written")), out, nil
	}

	authorName, authorEmail, err := toolAuthor(in.AuthorName, in.AuthorEmail)
	if err != nil {
		return nil, BatchEditsOutput{}, err
	}
	committed, err := s.commit(in.Message, authorName, authorEmail)
	if err != nil {
		return nil, BatchEditsOutput{}, err
	}
	out.Committed = committed

	status := "committed"
	if !committed {
		status = "no changes to commit"
	}
	return textResult("%s", renderBatch(out, status)), out, nil
}

func renderBatch(out BatchEditsOutput, status string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d file(s) — %s: %s\n", len(out.Files), status, out.Message)
	b.WriteString(renderLinks(out.Links))
	for _, f := range out.Files {
		verb := "updated"
		switch {
		case f.Deleted:
			verb = "deleted"
		case f.Created:
			verb = "created"
		case f.RenamedFrom != "":
			verb = "moved from " + f.RenamedFrom + " to"
		}
		fmt.Fprintf(&b, "\n--- %s %s ---\n%s", verb, f.Path, f.Diff)
	}
	return b.String()
}
