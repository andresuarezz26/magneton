package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupRepo creates a work repo with a bare origin and one commit on main.
func setupRepo(t *testing.T) (repo string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	repo = filepath.Join(root, "work")
	must := func(dir string, args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	must(root, "init", "-q", "--bare", origin)
	must(root, "init", "-q", repo)
	must(repo, "config", "user.email", "t@t.co")
	must(repo, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	must(repo, "add", "-A")
	must(repo, "commit", "-qm", "init")
	must(repo, "branch", "-M", "main")
	must(repo, "remote", "add", "origin", origin)
	must(repo, "push", "-qu", "origin", "main")
	return repo
}

func TestCreateWorktreeIdempotent(t *testing.T) {
	repo := setupRepo(t)
	wt := filepath.Join(t.TempDir(), "KAN-1")
	branch := "ai/kan-1-x"

	// First create succeeds and checks out a real worktree.
	if err := CreateWorktree(repo, wt, branch, "main"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if !fileExists(filepath.Join(wt, ".git")) || !fileExists(filepath.Join(wt, "README.md")) {
		t.Fatal("worktree not populated on first create")
	}

	// Re-create over the existing (registered) worktree: must still succeed.
	if err := CreateWorktree(repo, wt, branch, "main"); err != nil {
		t.Fatalf("re-create over registered worktree: %v", err)
	}

	// The real-world bug: a stale, UNREGISTERED leftover dir occupies the path
	// (branch lingers, dir has junk, no .git). Simulate it and re-create.
	_, _ = run(repo, "worktree", "remove", "--force", wt)
	_ = os.MkdirAll(filepath.Join(wt, ".idea"), 0o755) // leftover from an IDE
	_ = os.WriteFile(filepath.Join(wt, ".idea", "x"), []byte("junk"), 0o644)

	if err := CreateWorktree(repo, wt, branch, "main"); err != nil {
		t.Fatalf("re-create over stale leftover dir: %v", err)
	}
	if !fileExists(filepath.Join(wt, ".git")) || !fileExists(filepath.Join(wt, "README.md")) {
		t.Fatal("worktree not populated after clearing stale dir")
	}
}

func TestBranches(t *testing.T) {
	repo := setupRepo(t)
	must := func(dir string, args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Create a local-only branch to test dedup logic.
	must(repo, "checkout", "-b", "feature/local-only")
	must(repo, "checkout", "main")
	// Push a remote branch the local side doesn't have locally.
	must(repo, "push", "origin", "HEAD:refs/heads/feature/remote-only")
	must(repo, "fetch", "origin")

	branches, err := Branches(repo)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Branch{}
	for _, b := range branches {
		byName[b.Name] = b
	}

	// main: exists locally and remotely → should appear as local.
	if b, ok := byName["main"]; !ok || b.Remote {
		t.Errorf("main: want local, got %+v ok=%v", b, ok)
	}
	// local-only branch.
	if b, ok := byName["feature/local-only"]; !ok || b.Remote {
		t.Errorf("feature/local-only: want local, got %+v ok=%v", b, ok)
	}
	// remote-only branch.
	if b, ok := byName["feature/remote-only"]; !ok || !b.Remote {
		t.Errorf("feature/remote-only: want remote, got %+v ok=%v", b, ok)
	}
	// origin/HEAD sentinel must not appear.
	if _, ok := byName["HEAD"]; ok {
		t.Error("HEAD sentinel should be filtered out")
	}
}

func TestRefExists(t *testing.T) {
	repo := setupRepo(t)
	if !RefExists(repo, "main") {
		t.Error("main should exist")
	}
	if !RefExists(repo, "origin/main") {
		t.Error("origin/main should exist")
	}
	if RefExists(repo, "no-such-branch-xyz") {
		t.Error("no-such-branch-xyz should not exist")
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// CreateWorktree must exclude magneton's .agent/ scratch dir so it never lands
// in a commit - even though the agent writes report.json there and the
// orchestrator commits with `git add -A`.
func TestWorktreeExcludesAgentDir(t *testing.T) {
	repo := setupRepo(t)
	wt := filepath.Join(t.TempDir(), "KAN-1")
	if err := CreateWorktree(repo, wt, "ai/kan-1-x", "main"); err != nil {
		t.Fatal(err)
	}
	// The agent writes its report here; an ordinary source file lands too.
	if err := os.MkdirAll(filepath.Join(wt, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".agent", "report.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "Real.kt"), []byte("class Real"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CommitAll(wt, "work"); err != nil {
		t.Fatal(err)
	}
	files, err := run(wt, "show", "--name-only", "--pretty=format:", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(files, ".agent/") {
		t.Errorf(".agent/ leaked into the commit:\n%s", files)
	}
	if !strings.Contains(files, "Real.kt") {
		t.Errorf("expected the real source file to be committed, got:\n%s", files)
	}
}

func TestPushForceAllowsRerun(t *testing.T) {
	repo := setupRepo(t)
	wt := filepath.Join(t.TempDir(), "KAN-1")
	branch := "ai/kan-1-x"

	// First run: create worktree, commit, push (creates the remote branch).
	if err := CreateWorktree(repo, wt, branch, "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CommitAll(wt, "v1"); err != nil {
		t.Fatal(err)
	}
	if err := Push(wt, branch); err != nil {
		t.Fatalf("first push: %v", err)
	}

	// Fresh re-run: CreateWorktree resets the branch to origin/main (-B), new
	// commit → divergent history. A plain push is rejected (non-fast-forward);
	// force-with-lease must let it through.
	if err := CreateWorktree(repo, wt, branch, "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "b.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CommitAll(wt, "v2"); err != nil {
		t.Fatal(err)
	}
	if err := Push(wt, branch); err != nil {
		t.Fatalf("re-run push should force past the divergent remote branch: %v", err)
	}
}
