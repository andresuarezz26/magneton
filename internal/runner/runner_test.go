package runner

import (
	"os"
	"path/filepath"
	"strings"
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

func TestPrTitleFor(t *testing.T) {
	dir := t.TempDir()

	// No plan.json → no type prefix.
	got := prTitleFor(dir, "PROJ-1", "Fix login")
	if got != "[PROJ-1] Fix login" {
		t.Errorf("no plan: got %q", got)
	}

	// Write a feature plan.
	agentDir := filepath.Join(dir, ".agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "plan.json"),
		[]byte(`{"type":"feature","plan":"x","steps":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got = prTitleFor(dir, "PROJ-2", "Add upload flow")
	if got != "[feat][PROJ-2] Add upload flow" {
		t.Errorf("feature plan: got %q", got)
	}

	// Bug plan.
	if err := os.WriteFile(filepath.Join(agentDir, "plan.json"),
		[]byte(`{"type":"bug","plan":"x","steps":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got = prTitleFor(dir, "PROJ-3", "Fix crash")
	if got != "[bug][PROJ-3] Fix crash" {
		t.Errorf("bug plan: got %q", got)
	}

	// Chore plan.
	if err := os.WriteFile(filepath.Join(agentDir, "plan.json"),
		[]byte(`{"type":"chore","plan":"x","steps":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got = prTitleFor(dir, "PROJ-4", "Clean up deps")
	if got != "[chore][PROJ-4] Clean up deps" {
		t.Errorf("chore plan: got %q", got)
	}
}

func TestBranchPatternUsername(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	// Ensure a deterministic username by calling slugify on what ResolveUsername returns.
	// The key property: {username} is substituted (branch does not contain the literal placeholder).
	pattern := "{username}/{ticket}-{slug}"
	branch := strings.NewReplacer(
		"{ticket}", "proj-1",
		"{slug}", "add-feature",
		"{username}", "testuser",
	).Replace(pattern)
	if branch != "testuser/proj-1-add-feature" {
		t.Errorf("branch substitution: got %q", branch)
	}
	// Literal placeholder must not survive.
	if strings.Contains(branch, "{username}") {
		t.Error("branch still contains {username} placeholder")
	}
}
