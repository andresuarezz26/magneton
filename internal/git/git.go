// Package git manages per-ticket worktrees + branches (Decision 7).
package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// DefaultBranch returns origin's default branch (best effort).
func DefaultBranch(repo string) string {
	if out, err := run(repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(out, "origin/")
	}
	for _, b := range []string{"main", "master"} {
		if _, err := run(repo, "rev-parse", "--verify", "origin/"+b); err == nil {
			return b
		}
	}
	return "main"
}

// CreateWorktree fetches origin and adds a fresh worktree on a new branch off
// origin/<base>. A stale worktree at the same path is removed first.
func CreateWorktree(repo, worktreeDir, branch, base string) error {
	if _, err := run(repo, "fetch", "origin", "--prune"); err != nil {
		return err
	}
	if base == "" {
		base = DefaultBranch(repo)
	}
	if err := os.MkdirAll(filepath.Dir(worktreeDir), 0o755); err != nil {
		return err
	}
	_, _ = run(repo, "worktree", "remove", "--force", worktreeDir) // ignore if absent
	if _, err := run(repo, "worktree", "add", "-b", branch, worktreeDir, "origin/"+base); err != nil {
		// Branch may already exist (re-run): attach the existing branch.
		if _, err2 := run(repo, "worktree", "add", worktreeDir, branch); err2 != nil {
			return err
		}
	}
	return nil
}

// RemoveWorktree tears down a worktree (called on PR close in Phase 2).
func RemoveWorktree(repo, worktreeDir string) error {
	_, err := run(repo, "worktree", "remove", "--force", worktreeDir)
	return err
}

// HasChanges reports whether the worktree has uncommitted changes.
func HasChanges(worktreeDir string) bool {
	out, _ := run(worktreeDir, "status", "--porcelain")
	return strings.TrimSpace(out) != ""
}

// CommitAll stages and commits everything in the worktree.
func CommitAll(worktreeDir, msg string) error {
	if _, err := run(worktreeDir, "add", "-A"); err != nil {
		return err
	}
	_, err := run(worktreeDir, "commit", "-m", msg)
	return err
}

// Push pushes the branch to origin and sets upstream.
func Push(worktreeDir, branch string) error {
	_, err := run(worktreeDir, "push", "-u", "origin", branch)
	return err
}
