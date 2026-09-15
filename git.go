package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

// gitMu serializes git operations that touch the index so overlapping tool
// calls can't race. Read-only history commands don't take it.
var gitMu sync.Mutex

// refRe matches a git revision we're willing to hand to git: it must start with
// an alphanumeric (never '-', which would be read as an option) and contain only
// characters that appear in refs, short hashes, and relative syntax (HEAD~1,
// branch^). Range syntax ("a..b") is rejected here; callers pass endpoints
// separately.
var refRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/~^-]*$`)

// validRef reports whether s is a safe revision argument.
func validRef(s string) bool {
	return s != "" && !strings.Contains(s, "..") && refRe.MatchString(s)
}

// runGit runs a read-only `git -C root <args>` and returns stdout. On failure it
// returns stderr in the error. Not for commands that modify the index.
func runGit(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// isGitRepo reports whether root sits inside a git work tree.
func isGitRepo() bool {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--is-inside-work-tree").Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// toolAuthor validates and completes the attribution a *tool* call carries.
//
// The email is required of tools, and only of tools: an agent has no session for
// the server to recognise it by, so a call that doesn't name itself lands under
// whatever identity the vault repo is configured with, and an automated write
// becomes indistinguishable from a person's. A REST write is a different case —
// it arrives authenticated, and keeps the old fallback until attribution from
// the session is built (see BACKLOG.md).
//
// The name defaults to the local part of the email, so a caller that has already
// said who it is need not say it twice.
func toolAuthor(name, email string) (string, string, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", "", fmt.Errorf("author_email is required: name who or what is making this change, so the commit is attributable")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = email
		if at := strings.Index(email, "@"); at > 0 {
			name = email[:at]
		}
	}
	return name, email, nil
}

// requireCommitted returns an error wrapping errUncommitted unless abs is
// tracked and identical to HEAD, in both the index and the working tree.
func requireCommitted(abs string) error {
	if _, err := runGit("ls-files", "--error-unmatch", "--", abs); err != nil {
		return fmt.Errorf("%s is not tracked by git: %w", relPath(abs), errUncommitted)
	}
	// Any status line — staged, modified, or a type change — means the file on
	// disk is not what HEAD holds.
	out, err := runGit("status", "--porcelain", "--", abs)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("%s has uncommitted changes: %w", relPath(abs), errUncommitted)
	}
	return nil
}

// gitCommit stages the given paths and commits exactly them with message,
// leaving any other staged changes untouched (pathspec-limited commit). When
// authorName/authorEmail are non-empty they override the committer identity for
// this commit (via `-c user.name/-c user.email`), so multiple agents show up
// distinctly in the history; git fills any unset half from config.
//
// It reports whether a commit was actually made: if the paths carry no change
// relative to HEAD there is nothing to commit and committed is false with a nil
// error. Callers must have already written the files to disk.
func gitCommit(paths []string, message, authorName, authorEmail string) (committed bool, err error) {
	// With no paths, the `--` pathspecs below would match everything, and this
	// would commit whatever else happens to be staged.
	if len(paths) == 0 {
		return false, nil
	}
	gitMu.Lock()
	defer gitMu.Unlock()

	addArgs := append([]string{"-C", root, "add", "--"}, paths...)
	if out, err := exec.Command("git", addArgs...).CombinedOutput(); err != nil {
		return false, fmt.Errorf("git add: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// If nothing changed for these paths, there is nothing to commit. A clean
	// `git diff --cached --quiet` exits 0; a difference exits non-zero.
	diffArgs := append([]string{"-C", root, "diff", "--cached", "--quiet", "--"}, paths...)
	if exec.Command("git", diffArgs...).Run() == nil {
		return false, nil
	}

	commitArgs := []string{"-C", root}
	if authorName != "" {
		commitArgs = append(commitArgs, "-c", "user.name="+authorName)
	}
	if authorEmail != "" {
		commitArgs = append(commitArgs, "-c", "user.email="+authorEmail)
	}
	commitArgs = append(commitArgs, "commit", "-m", message, "--")
	commitArgs = append(commitArgs, paths...)
	if out, err := exec.Command("git", commitArgs...).CombinedOutput(); err != nil {
		return false, fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}
