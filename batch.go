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
// ("write"), replacing a substring in an existing one ("edit"), or removing one
// ("delete").
type BatchOp struct {
	Op         string `json:"op" jsonschema:"'write' to create or overwrite a file with content, 'edit' to replace old_string with new_string, or 'delete' to remove a file (only if its current content is committed; a symlink is removed itself)"`
	Path       string `json:"path" jsonschema:"file to act on, relative to root"`
	Content    string `json:"content,omitempty" jsonschema:"for op=write: full new contents of the file"`
	OldString  string `json:"old_string,omitempty" jsonschema:"for op=edit: exact text to replace; must occur exactly once unless replace_all is set"`
	NewString  string `json:"new_string,omitempty" jsonschema:"for op=edit: text to replace it with"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"for op=edit: replace every occurrence instead of requiring a unique match"`
}

type BatchEditsInput struct {
	Message     string    `json:"message" jsonschema:"commit message for the whole batch (required)"`
	Ops         []BatchOp `json:"ops" jsonschema:"ordered list of write/edit/delete operations, applied in order and committed together as one commit"`
	AuthorName  string    `json:"author_name,omitempty" jsonschema:"name to attribute the commit to; defaults to the part of author_email before the @"`
	AuthorEmail string    `json:"author_email" jsonschema:"email to attribute the commit to (required): who or what is making this change, e.g. the agent's own address. Every commit names an author, so an automated writer stays distinguishable from a person"`
	DryRun      bool      `json:"dry_run,omitempty" jsonschema:"if true, validate and return the diffs without writing or committing anything"`
}

type BatchFileResult struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
	Deleted bool   `json:"deleted"`
	Diff    string `json:"diff"`
}

type BatchEditsOutput struct {
	Message   string            `json:"message"`
	DryRun    bool              `json:"dry_run"`
	Committed bool              `json:"committed"`
	Files     []BatchFileResult `json:"files"`
}

// batchFile is one file's working state while a batch is validated.
type batchFile struct {
	orig    string // on-disk content before the batch (diff base)
	cur     string // content after the ops so far
	existed bool   // on disk before the batch
	exists  bool   // present after the ops so far
}

// BatchEdits applies a mix of write, edit, and delete operations atomically:
// every op is validated against the evolving in-memory state first, so a single
// bad op aborts the whole batch without touching disk. Only if all ops are valid
// are the files written or removed and committed together in one commit.
func BatchEdits(ctx context.Context, req *mcp.CallToolRequest, in BatchEditsInput) (*mcp.CallToolResult, BatchEditsOutput, error) {
	if strings.TrimSpace(in.Message) == "" {
		return nil, BatchEditsOutput{}, fmt.Errorf("message is required")
	}
	if len(in.Ops) == 0 {
		return nil, BatchEditsOutput{}, fmt.Errorf("ops must not be empty")
	}

	// Keyed by resolved absolute path, so two spellings of one file share one
	// state. order records first-touch order for stable output.
	files := map[string]*batchFile{}
	var order []string
	track := func(p string, f *batchFile) {
		files[p] = f
		order = append(order, p)
	}

	// load returns the state of the file a write or edit targets.
	load := func(rel string) (*batchFile, error) {
		p, err := resolve(rel)
		if err != nil {
			return nil, err
		}
		if f, seen := files[p]; seen {
			return f, nil
		}
		info, statErr := os.Stat(p)
		if statErr != nil {
			// Treat a missing file as empty/new; any other stat error is real.
			if os.IsNotExist(statErr) {
				f := &batchFile{}
				track(p, f)
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
		track(p, f)
		return f, nil
	}

	// Validate + apply in memory.
	for i, op := range in.Ops {
		switch op.Op {
		case "write":
			f, err := load(op.Path)
			if err != nil {
				return nil, BatchEditsOutput{}, fmt.Errorf("op %d (write %s): %w", i, op.Path, err)
			}
			f.cur = op.Content
			f.exists = true
		case "edit":
			if op.OldString == "" {
				return nil, BatchEditsOutput{}, fmt.Errorf("op %d (edit %s): old_string must not be empty", i, op.Path)
			}
			if op.OldString == op.NewString {
				return nil, BatchEditsOutput{}, fmt.Errorf("op %d (edit %s): old_string and new_string are identical", i, op.Path)
			}
			f, err := load(op.Path)
			if err != nil {
				return nil, BatchEditsOutput{}, fmt.Errorf("op %d (edit %s): %w", i, op.Path, err)
			}
			if !f.exists {
				return nil, BatchEditsOutput{}, fmt.Errorf("op %d (edit %s): no such file", i, op.Path)
			}
			count := strings.Count(f.cur, op.OldString)
			switch {
			case count == 0:
				return nil, BatchEditsOutput{}, fmt.Errorf("op %d (edit %s): old_string not found", i, op.Path)
			case count > 1 && !op.ReplaceAll:
				return nil, BatchEditsOutput{}, fmt.Errorf("op %d (edit %s): old_string occurs %d times; add context to make it unique, or set replace_all", i, op.Path, count)
			}
			if op.ReplaceAll {
				f.cur = strings.ReplaceAll(f.cur, op.OldString, op.NewString)
			} else {
				f.cur = strings.Replace(f.cur, op.OldString, op.NewString, 1)
			}
		case "delete":
			p, err := resolveLeaf(op.Path)
			if err != nil {
				return nil, BatchEditsOutput{}, fmt.Errorf("op %d (delete %s): %w", i, op.Path, err)
			}
			f, seen := files[p]
			switch {
			case !seen:
				old, err := prepareDelete(p)
				if err != nil {
					return nil, BatchEditsOutput{}, fmt.Errorf("op %d (delete %s): %w", i, op.Path, err)
				}
				f = &batchFile{orig: old, existed: true}
				track(p, f)
			case !f.exists:
				return nil, BatchEditsOutput{}, fmt.Errorf("op %d (delete %s): no such file", i, op.Path)
			case f.existed:
				// Earlier ops only changed it in memory; what the deletion would
				// discard is still what is on disk, so that is what must be
				// committed.
				if err := requireCommitted(p); err != nil {
					return nil, BatchEditsOutput{}, fmt.Errorf("op %d (delete %s): %w", i, op.Path, err)
				}
			}
			f.cur = ""
			f.exists = false
		default:
			return nil, BatchEditsOutput{}, fmt.Errorf("op %d: unknown op %q (want 'write', 'edit', or 'delete')", i, op.Op)
		}
	}

	out := BatchEditsOutput{Message: in.Message, DryRun: in.DryRun}
	for _, p := range order {
		f := files[p]
		if !f.existed && !f.exists {
			continue // created and deleted within the batch: nothing changes on disk
		}
		out.Files = append(out.Files, BatchFileResult{
			Path:    relPath(p),
			Created: !f.existed,
			Deleted: !f.exists,
			Diff:    diffText(f.orig, f.cur, 3),
		})
	}

	if in.DryRun {
		return textResult("%s", renderBatch(out, "dry run, nothing written")), out, nil
	}

	authorName, authorEmail, err := toolAuthor(in.AuthorName, in.AuthorEmail)
	if err != nil {
		return nil, BatchEditsOutput{}, err
	}

	// Write or remove every touched file, then commit them all in one commit.
	var paths []string
	for _, p := range order {
		f := files[p]
		switch {
		case f.exists:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return nil, BatchEditsOutput{}, err
			}
			if err := os.WriteFile(p, []byte(f.cur), 0o644); err != nil {
				return nil, BatchEditsOutput{}, err
			}
		case f.existed:
			if err := os.Remove(p); err != nil {
				return nil, BatchEditsOutput{}, err
			}
			pruneEmptyDirs(filepath.Dir(p))
		default:
			continue
		}
		paths = append(paths, p)
	}

	committed, err := gitCommit(paths, in.Message, authorName, authorEmail)
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
	for _, f := range out.Files {
		verb := "updated"
		switch {
		case f.Deleted:
			verb = "deleted"
		case f.Created:
			verb = "created"
		}
		fmt.Fprintf(&b, "\n--- %s %s ---\n%s", verb, f.Path, f.Diff)
	}
	return b.String()
}
