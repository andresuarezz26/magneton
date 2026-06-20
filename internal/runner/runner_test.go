package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/store"
)

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Remove deprecated lint baseline entries": "remove-deprecated-lint-baseline-entries",
		"Fix NPE in LoginActivity!!!":             "fix-npe-in-loginactivity",
		"   ":                                     "change",
		"Bump kotlin 1.9 → 2.0":                   "bump-kotlin-1-9-2-0",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWorktreeReady(t *testing.T) {
	dir := t.TempDir()
	if worktreeReady(dir) {
		t.Error("empty dir should not be a ready worktree")
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !worktreeReady(dir) {
		t.Error("dir with a .git link should be a ready worktree")
	}
}

func TestResumeRefusesWithoutWorktree(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	out := Run(Task{
		Ticket:  "NOPE-1",
		Summary: "x",
		Resume:  true,
		Repo:    &config.Repo{Branch: "ai/{ticket}-{slug}"},
		Cfg:     &config.Config{},
	}, Hooks{})
	if out.State != store.StateFailed || out.Err == nil {
		t.Errorf("resume with no worktree should fail fast, got state=%q err=%v", out.State, out.Err)
	}
}
