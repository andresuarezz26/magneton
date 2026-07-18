// Package paths centralizes the ~/.magneton directory layout.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Root is ~/.magneton (override with $MAGNETON_HOME, mainly for tests).
func Root() string {
	if v := os.Getenv("MAGNETON_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".magneton")
}

func Config() string    { return filepath.Join(Root(), "config.toml") }
func StateDB() string   { return filepath.Join(Root(), "state.db") }
func Worktrees() string { return filepath.Join(Root(), "worktrees") }
func Logs() string      { return filepath.Join(Root(), "logs") }
func Templates() string { return filepath.Join(Root(), "templates") }
func Reports() string   { return filepath.Join(Root(), "reports") }
func Pasted() string    { return filepath.Join(Root(), "pasted") }
func DaemonLog() string { return filepath.Join(Root(), "daemon.log") }
func PidFile() string   { return filepath.Join(Root(), "daemon.pid") }

// WorktreeFor returns a ticket's worktree path under ~/.magneton/worktrees/<ticket>.
// All worktrees live in magneton's own home so they're easy to find regardless
// of which repo the ticket came from.
func WorktreeFor(_, ticket string) string {
	return filepath.Join(Worktrees(), ticket)
}

func LogFor(ticket string) string { return filepath.Join(Logs(), ticket+".log") }

// ReportFor is where magneton archives a ticket's completion report, kept in
// magneton's own home so it never lands in the target repo / PR.
func ReportFor(ticket string) string { return filepath.Join(Reports(), ticket+".json") }

// WriteLocalProperties writes sdk.dir to <dir>/local.properties so Gradle can
// locate the Android SDK in fresh worktrees where the file is git-ignored.
// No-op when sdkPath is empty.
func WriteLocalProperties(dir, sdkPath string) error {
	if sdkPath == "" {
		return nil
	}
	return os.WriteFile(
		filepath.Join(dir, "local.properties"),
		[]byte(fmt.Sprintf("sdk.dir=%s\n", sdkPath)),
		0o644,
	)
}

// migrateAgentDir renames oldRoot to newRoot when oldRoot exists and newRoot does not.
func migrateAgentDir(oldRoot, newRoot string) {
	if _, err := os.Stat(oldRoot); err != nil {
		return // old dir absent — nothing to migrate
	}
	if _, err := os.Stat(newRoot); !os.IsNotExist(err) {
		return // new dir already exists — don't overwrite
	}
	if err := os.Rename(oldRoot, newRoot); err == nil {
		fmt.Fprintf(os.Stderr, "magneton: migrated ~/.agent → ~/.magneton\n")
	}
}

// EnsureDirs creates the directory skeleton if missing.
// On first run after upgrading to 2.0 it migrates the old ~/.agent directory
// to ~/.magneton (only when MAGNETON_HOME is not overridden).
func EnsureDirs() error {
	root := Root()
	if os.Getenv("MAGNETON_HOME") == "" {
		home, _ := os.UserHomeDir()
		migrateAgentDir(filepath.Join(home, ".agent"), root)
	}
	for _, d := range []string{root, Worktrees(), Logs(), Templates(), Reports(), Pasted()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
