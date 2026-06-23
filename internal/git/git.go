// Package git manages per-ticket worktrees + branches (Decision 7).
package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// repoLocks serializes CreateWorktree calls per repo path. git worktree add
// and git fetch both write to .git/config and can't run concurrently on the
// same repo without hitting "could not lock config file .git/config".
var repoLocks sync.Map // map[string]*sync.Mutex

func repoLock(repo string) *sync.Mutex {
	v, _ := repoLocks.LoadOrStore(repo, &sync.Mutex{})
	return v.(*sync.Mutex)
}

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
// The call is serialized per repo to avoid concurrent writes to .git/config.
func CreateWorktree(repo, worktreeDir, branch, base string) error {
	mu := repoLock(repo)
	mu.Lock()
	defer mu.Unlock()

	if _, err := run(repo, "fetch", "origin", "--prune"); err != nil {
		return err
	}
	if base == "" {
		base = DefaultBranch(repo)
	}
	if err := os.MkdirAll(filepath.Dir(worktreeDir), 0o755); err != nil {
		return err
	}
	_, _ = run(repo, "worktree", "remove", "--force", worktreeDir) // de-register if registered
	_, _ = run(repo, "worktree", "prune")                          // drop stale admin entries
	// Nuke any leftover directory still occupying the path (e.g. a stale .idea/
	// left by opening the worktree in an IDE). git refuses to add a worktree onto
	// a non-empty dir, and `worktree remove` can't clear an unregistered one.
	if err := os.RemoveAll(worktreeDir); err != nil {
		return fmt.Errorf("clear stale worktree dir: %w", err)
	}
	// -B creates the branch, or RESETS it to origin/<base> if it already exists,
	// so a re-run starts clean whether or not the branch lingers from a prior run.
	if _, err := run(repo, "worktree", "add", "-B", branch, worktreeDir, "origin/"+base); err != nil {
		return err
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

// Push force-pushes the branch to origin (with lease) and sets upstream. The
// ai/<ticket>-<slug> branches are magneton-owned and regenerated each run, so a
// fresh re-run rewrites history and a plain push would be rejected as non-fast-
// forward. --force-with-lease overwrites the prior remote branch safely:
// CreateWorktree fetches first, so the lease reflects the current remote.
func Push(worktreeDir, branch string) error {
	_, err := run(worktreeDir, "push", "--force-with-lease", "-u", "origin", branch)
	return err
}
