// Package paths centralizes the ~/.agent directory layout (Decision 15).
package paths

import (
	"os"
	"path/filepath"
)

// Root is ~/.agent (override with $DROIDPILOT_HOME, mainly for tests).
func Root() string {
	if v := os.Getenv("DROIDPILOT_HOME"); v != "" {
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
func DaemonLog() string { return filepath.Join(Root(), "daemon.log") }
func PidFile() string    { return filepath.Join(Root(), "daemon.pid") }

func WorktreeFor(ticket string) string   { return filepath.Join(Worktrees(), ticket) }
func GradleHomeFor(ticket string) string { return filepath.Join(WorktreeFor(ticket), ".gradle-home") }
func LogFor(ticket string) string        { return filepath.Join(Logs(), ticket+".log") }

// EnsureDirs creates the directory skeleton if missing.
func EnsureDirs() error {
	for _, d := range []string{Root(), Worktrees(), Logs(), Templates()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
