package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- delete_file ----

// errUncommitted marks a refused deletion of a file whose current content is not
// in git. Deleting is only safe because history can bring the file back; an
// untracked file, or uncommitted changes to a tracked one, would be lost for
// good.
var errUncommitted = errors.New("only a file whose current content is committed can be deleted, so the deletion can be undone from history")

type DeleteFileInput struct {
	Path        string `json:"path" jsonschema:"file to delete, relative to root; a symlink is removed itself, not the file it points to"`
	Message     string `json:"message" jsonschema:"commit message (required); the removal is committed"`
	AuthorName  string `json:"author_name,omitempty" jsonschema:"name to attribute the commit to; defaults to the part of author_email before the @"`
	AuthorEmail string `json:"author_email" jsonschema:"email to attribute the commit to (required): who or what is making this change, e.g. the agent's own address. Every commit names an author, so an automated writer stays distinguishable from a person"`
	DryRun      bool   `json:"dry_run,omitempty" jsonschema:"if true, check the deletion and return the diff without deleting or committing anything"`
}

type DeleteFileOutput struct {
	Path      string `json:"path"`
	DryRun    bool   `json:"dry_run"`
	Committed bool   `json:"committed"`
	Diff      string `json:"diff"`
}

// resolveLeaf is resolve for acting on a directory entry itself rather than on
// what it points to: the parent is resolved and confined as usual, but a symlink
// in the last segment is not followed. Deleting a symlinked note then removes
// the link, as rm does — which is also how git sees it: a link is its own
// tracked entry (mode 120000, content = target path), not the file it names.
func resolveLeaf(rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must be relative to the served folder: %q", rel)
	}
	dir, base := filepath.Split(filepath.Clean(rel))
	if base == "." || base == ".." {
		return "", fmt.Errorf("path does not name a file: %q", rel)
	}
	if isHidden(base) {
		return "", fmt.Errorf("hidden path is not served: %q", rel)
	}
	parent, err := resolve(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, base), nil
}

// prepareDelete checks that the entry at abs (from resolveLeaf) may be deleted
// and returns the content the removal diff is taken from: the text of a file,
// the target of a symlink (which is what git diffs for a link), or "" for a
// binary file.
func prepareDelete(abs string) (old string, err error) {
	info, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s: no such file: %w", relPath(abs), fs.ErrNotExist)
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory; only files can be deleted", relPath(abs))
	}
	if err := requireCommitted(abs); err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return os.Readlink(abs)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	if looksBinary(data[:min(len(data), 512)]) {
		return "", nil
	}
	return string(data), nil
}

// pruneEmptyDirs removes dir, then each parent left empty by that, stopping at
// root. Git has no notion of a folder, so one emptied by a deletion would linger
// in listings with nothing in history to explain it. os.Remove refuses a
// non-empty directory, which is what ends the walk.
func pruneEmptyDirs(dir string) {
	for strings.HasPrefix(dir, root+string(filepath.Separator)) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// deleteOutcome is the result of deleteAndCommit.
type deleteOutcome struct {
	Abs       string // resolved absolute path removed
	Old       string // content before removal (for diffing), see prepareDelete
	Committed bool   // a commit was actually made
}

// deleteAndCommit removes the file at rel, prunes folders it leaves empty, and
// commits the removal. It is the shared core behind delete_file and REST DELETE.
//
// The commit check and the removal are not atomic with a concurrent write; the
// worst case is a write committed just before the deletion, which history still
// holds.
func deleteAndCommit(rel, message, authorName, authorEmail string) (deleteOutcome, error) {
	if strings.TrimSpace(message) == "" {
		return deleteOutcome{}, fmt.Errorf("message is required")
	}
	abs, err := resolveLeaf(rel)
	if err != nil {
		return deleteOutcome{}, err
	}
	old, err := prepareDelete(abs)
	if err != nil {
		return deleteOutcome{}, err
	}
	if err := os.Remove(abs); err != nil {
		return deleteOutcome{}, err
	}
	pruneEmptyDirs(filepath.Dir(abs))
	committed, err := gitCommit([]string{abs}, message, authorName, authorEmail)
	if err != nil {
		return deleteOutcome{}, err
	}
	return deleteOutcome{Abs: abs, Old: old, Committed: committed}, nil
}

func DeleteFile(ctx context.Context, req *mcp.CallToolRequest, in DeleteFileInput) (*mcp.CallToolResult, DeleteFileOutput, error) {
	if strings.TrimSpace(in.Message) == "" {
		return nil, DeleteFileOutput{}, fmt.Errorf("message is required")
	}
	abs, err := resolveLeaf(in.Path)
	if err != nil {
		return nil, DeleteFileOutput{}, err
	}

	if in.DryRun {
		old, err := prepareDelete(abs)
		if err != nil {
			return nil, DeleteFileOutput{}, err
		}
		diff := diffText(old, "", 3)
		out := DeleteFileOutput{Path: relPath(abs), DryRun: true, Diff: diff}
		return textResult("dry run, nothing deleted.\n--- diff ---\n%s", diff), out, nil
	}

	authorName, authorEmail, err := toolAuthor(in.AuthorName, in.AuthorEmail)
	if err != nil {
		return nil, DeleteFileOutput{}, err
	}

	res, err := deleteAndCommit(in.Path, in.Message, authorName, authorEmail)
	if err != nil {
		return nil, DeleteFileOutput{}, err
	}

	diff := diffText(res.Old, "", 3)
	out := DeleteFileOutput{Path: relPath(res.Abs), Committed: res.Committed, Diff: diff}
	status := "committed"
	if !res.Committed {
		status = "no changes to commit"
	}
	return textResult("deleted %s — %s.\n--- diff ---\n%s", relPath(res.Abs), status, diff), out, nil
}
