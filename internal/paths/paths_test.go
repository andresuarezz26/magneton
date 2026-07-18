package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootUsesMagneton(t *testing.T) {
	t.Setenv("MAGNETON_HOME", "")
	root := Root()
	if !strings.HasSuffix(root, ".magneton") {
		t.Errorf("Root() = %q, want path ending in .magneton", root)
	}
}

func TestRootEnvOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MAGNETON_HOME", tmp)
	if got := Root(); got != tmp {
		t.Errorf("Root() = %q, want %q", got, tmp)
	}
}

func TestWorktreeForAlwaysUnderRoot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MAGNETON_HOME", tmp)

	// non-empty repo: still uses magneton home
	got := WorktreeFor("/some/android/repo", "PROJ-1")
	want := filepath.Join(tmp, "worktrees", "PROJ-1")
	if got != want {
		t.Errorf("WorktreeFor(repo, ticket) = %q, want %q", got, want)
	}

	// empty repo: same result
	got2 := WorktreeFor("", "PROJ-1")
	if got2 != want {
		t.Errorf("WorktreeFor('', ticket) = %q, want %q", got2, want)
	}
}

func TestEnsureDirsMigration(t *testing.T) {
	tmp := t.TempDir()
	oldRoot := filepath.Join(tmp, ".agent")
	newRoot := filepath.Join(tmp, ".magneton")

	// Create the old directory with a sentinel file.
	if err := os.MkdirAll(filepath.Join(oldRoot, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(oldRoot, "logs", "test.log")
	if err := os.WriteFile(sentinel, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Override both home and MAGNETON_HOME to point into our temp dir.
	// EnsureDirs only runs migration when MAGNETON_HOME is unset, so we
	// test the migration logic directly by calling the internal helper via
	// a fake home injection approach: set home via env simulation is not
	// portable. Instead, call the migration snippet directly via a helper.
	migrateAgentDir(oldRoot, newRoot)

	// Old dir should be gone; new dir should exist with the sentinel.
	if _, err := os.Stat(oldRoot); !os.IsNotExist(err) {
		t.Errorf("old root still exists after migration")
	}
	moved := filepath.Join(newRoot, "logs", "test.log")
	if _, err := os.Stat(moved); err != nil {
		t.Errorf("sentinel not found in new root: %v", err)
	}
}

func TestEnsureDirsMigrationSkipsIfNewExists(t *testing.T) {
	tmp := t.TempDir()
	oldRoot := filepath.Join(tmp, ".agent")
	newRoot := filepath.Join(tmp, ".magneton")

	// Both exist — migration must not overwrite newRoot.
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newRoot, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	migrateAgentDir(oldRoot, newRoot)

	// New root should still have its file; old root should still exist too.
	if _, err := os.Stat(filepath.Join(newRoot, "keep.txt")); err != nil {
		t.Errorf("newRoot file gone: %v", err)
	}
	if _, err := os.Stat(oldRoot); err != nil {
		t.Errorf("oldRoot should still exist when migration skipped: %v", err)
	}
}
