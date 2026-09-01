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

// BatchOp is one operation in a batch: either creating/overwriting a whole file
// ("write") or replacing a substring in an existing one ("edit").
type BatchOp struct {
	Op         string `json:"op" jsonschema:"either 'write' to create or overwrite a file with content, or 'edit' to replace old_string with new_string"`
	Path       string `json:"path" jsonschema:"file to act on, relative to root"`
	Content    string `json:"content,omitempty" jsonschema:"for op=write: full new contents of the file"`
	OldString  string `json:"old_string,omitempty" jsonschema:"for op=edit: exact text to replace; must occur exactly once unless replace_all is set"`
	NewString  string `json:"new_string,omitempty" jsonschema:"for op=edit: text to replace it with"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"for op=edit: replace every occurrence instead of requiring a unique match"`
}

type BatchEditsInput struct {
	Message     string    `json:"message" jsonschema:"commit message for the whole batch (required)"`
	Ops         []BatchOp `json:"ops" jsonschema:"ordered list of write/edit operations, applied in order and committed together as one commit"`
	AuthorName  string    `json:"author_name,omitempty" jsonschema:"name to attribute the commit to (e.g. the agent making the change); defaults to the repo's configured identity"`
	AuthorEmail string    `json:"author_email,omitempty" jsonschema:"email to attribute the commit to; defaults to the repo's configured identity"`
	DryRun      bool      `json:"dry_run,omitempty" jsonschema:"if true, validate and return the diffs without writing or committing anything"`
}

type BatchFileResult struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
	Diff    string `json:"diff"`
}

type BatchEditsOutput struct {
	Message   string            `json:"message"`
	DryRun    bool              `json:"dry_run"`
	Committed bool              `json:"committed"`
	Files     []BatchFileResult `json:"files"`
}

// BatchEdits applies a mix of write and edit operations atomically: every op is
// validated against the evolving in-memory contents first, so a single bad op
// aborts the whole batch without writing anything. Only if all ops are valid
// are the files written and committed together in one commit.
func BatchEdits(ctx context.Context, req *mcp.CallToolRequest, in BatchEditsInput) (*mcp.CallToolResult, BatchEditsOutput, error) {
	if strings.TrimSpace(in.Message) == "" {
		return nil, BatchEditsOutput{}, fmt.Errorf("message is required")
	}
	if len(in.Ops) == 0 {
		return nil, BatchEditsOutput{}, fmt.Errorf("ops must not be empty")
	}

	// Per-path working state. orig holds the original on-disk content (diff
	// base); cur holds the evolving content as ops are applied; order records
	// first-touch order for stable output. abs maps a path to its resolved
	// absolute path.
	orig := map[string]string{}
	cur := map[string]string{}
	created := map[string]bool{}
	abs := map[string]string{}
	var order []string

	load := func(rel string) (string, error) {
		p, err := resolve(rel)
		if err != nil {
			return "", err
		}
		if _, seen := cur[rel]; seen {
			return p, nil
		}
		abs[rel] = p
		order = append(order, rel)
		info, statErr := os.Stat(p)
		if statErr != nil {
			// Treat a missing file as empty/new; a real stat error (e.g. it's a
			// directory) is reported below.
			if os.IsNotExist(statErr) {
				orig[rel] = ""
				cur[rel] = ""
				created[rel] = true
				return p, nil
			}
			return "", statErr
		}
		if info.IsDir() {
			return "", fmt.Errorf("%s is a directory", rel)
		}
		data, rdErr := os.ReadFile(p)
		if rdErr != nil {
			return "", rdErr
		}
		if looksBinary(data[:min(len(data), 512)]) {
			return "", fmt.Errorf("%s appears to be a binary file", rel)
		}
		orig[rel] = string(data)
		cur[rel] = string(data)
		created[rel] = false
		return p, nil
	}

	// Validate + apply in memory.
	for i, op := range in.Ops {
		switch op.Op {
		case "write":
			if _, err := load(op.Path); err != nil {
				return nil, BatchEditsOutput{}, fmt.Errorf("op %d (write %s): %w", i, op.Path, err)
			}
			cur[op.Path] = op.Content
		case "edit":
			if op.OldString == "" {
				return nil, BatchEditsOutput{}, fmt.Errorf("op %d (edit %s): old_string must not be empty", i, op.Path)
			}
			if op.OldString == op.NewString {
				return nil, BatchEditsOutput{}, fmt.Errorf("op %d (edit %s): old_string and new_string are identical", i, op.Path)
			}
			if _, err := load(op.Path); err != nil {
				return nil, BatchEditsOutput{}, fmt.Errorf("op %d (edit %s): %w", i, op.Path, err)
			}
			content := cur[op.Path]
			count := strings.Count(content, op.OldString)
			switch {
			case count == 0:
				return nil, BatchEditsOutput{}, fmt.Errorf("op %d (edit %s): old_string not found", i, op.Path)
			case count > 1 && !op.ReplaceAll:
				return nil, BatchEditsOutput{}, fmt.Errorf("op %d (edit %s): old_string occurs %d times; add context to make it unique, or set replace_all", i, op.Path, count)
			}
			if op.ReplaceAll {
				cur[op.Path] = strings.ReplaceAll(content, op.OldString, op.NewString)
			} else {
				cur[op.Path] = strings.Replace(content, op.OldString, op.NewString, 1)
			}
		default:
			return nil, BatchEditsOutput{}, fmt.Errorf("op %d: unknown op %q (want 'write' or 'edit')", i, op.Op)
		}
	}

	out := BatchEditsOutput{Message: in.Message, DryRun: in.DryRun}
	for _, rel := range order {
		out.Files = append(out.Files, BatchFileResult{
			Path:    relPath(abs[rel]),
			Created: created[rel],
			Diff:    diffText(orig[rel], cur[rel], 3),
		})
	}

	if in.DryRun {
		return textResult("%s", renderBatch(out, "dry run, nothing written")), out, nil
	}

	// Write every touched file, then commit them all in one commit.
	var paths []string
	for _, rel := range order {
		p := abs[rel]
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, BatchEditsOutput{}, err
		}
		if err := os.WriteFile(p, []byte(cur[rel]), 0o644); err != nil {
			return nil, BatchEditsOutput{}, err
		}
		paths = append(paths, p)
	}

	committed, err := gitCommit(paths, in.Message, in.AuthorName, in.AuthorEmail)
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
		if f.Created {
			verb = "created"
		}
		fmt.Fprintf(&b, "\n--- %s %s ---\n%s", verb, f.Path, f.Diff)
	}
	return b.String()
}
