// Package paths centralizes the ~/.agent directory layout (Decision 15).
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Root is ~/.agent (override with $MAGNETON_HOME, mainly for tests).
func Root() string {
	if v := os.Getenv("MAGNETON_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agent")
}

func Config() string    { return filepath.Join(Root(), "config.toml") }
func StateDB() string   { return filepath.Join(Root(), "state.db") }
func Worktrees() string { return filepath.Join(Root(), "worktrees") }
func Logs() string      { return filepath.Join(Root(), "logs") }
func Templates() string { return filepath.Join(Root(), "templates") }
func Reports() string   { return filepath.Join(Root(), "reports") }
func DaemonLog() string { return filepath.Join(Root(), "daemon.log") }
func PidFile() string    { return filepath.Join(Root(), "daemon.pid") }

// WorktreeFor returns a ticket's worktree path. To mirror how native
// `git worktree` (and Claude Code's `claude worktree`) place worktrees next to
// the project rather than off in a hidden home dir, it sits in a sibling
// directory of the repo: "<repo-parent>/<repo>-worktrees/<ticket>". This keeps a
// ticket's checkout adjacent to its repo (easy to find / open in an IDE) while
// staying outside the repo so the parent's git never sees it. Falls back to the
// agent home (~/.agent/worktrees/<ticket>) when repo is empty.
func WorktreeFor(repo, ticket string) string {
	if repo == "" {
		return filepath.Join(Worktrees(), ticket)
	}
	repo = filepath.Clean(repo)
	return filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-worktrees", ticket)
}

func GradleHomeFor(_ string) string { return filepath.Join(Root(), ".gradle-home") }
func LogFor(ticket string) string      { return filepath.Join(Logs(), ticket+".log") }

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

// EnsureDirs creates the directory skeleton if missing.
func EnsureDirs() error {
	for _, d := range []string{Root(), Worktrees(), Logs(), Templates(), Reports()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
