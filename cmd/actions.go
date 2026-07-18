package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/store"
)

// shellQuote single-quotes a string for safe embedding in a shell command line.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// projectDirIn maps the configured repo path to its location inside a worktree.
// When the repo path is a subdirectory of its git repository (monorepo: git
// root at /repo, Android project at /repo/App), the worktree mirrors the whole
// repository, so the project sits at the same relative offset - Android Studio
// must open <worktree>/App, not the worktree root. Falls back to the worktree
// root when the repo path IS the git root or anything fails to resolve.
func projectDirIn(worktreeDir, repo string) string {
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return worktreeDir
	}
	// git resolves symlinks in --show-toplevel; resolve the configured path the
	// same way or Rel would mismatch (e.g. /var vs /private/var on macOS).
	top := strings.TrimSpace(string(out))
	repoPath, err := filepath.EvalSymlinks(filepath.Clean(repo))
	if err != nil {
		return worktreeDir
	}
	rel, err := filepath.Rel(top, repoPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return worktreeDir
	}
	if fi, err := os.Stat(filepath.Join(worktreeDir, rel)); err == nil && fi.IsDir() {
		return filepath.Join(worktreeDir, rel)
	}
	return worktreeDir
}

// worktreeExists reports whether the ticket still has a usable git worktree.
// "Stop & clean up" removes it, so Resume / Open Studio / Open Claude only make
// sense when this is true.
func worktreeExists(repo, ticket string) bool {
	_, err := os.Stat(filepath.Join(paths.WorktreeFor(repo, ticket), ".git"))
	return err == nil
}

// agentActions returns the menu items that make sense for one agent's state and
// whether its worktree still exists. Shown when the user presses Enter on it.
func agentActions(s store.Session) []paletteItem {
	var items []paletteItem
	hasWT := worktreeExists(s.Repo, s.Ticket)
	active := store.IsActive(s.State)
	stuck := s.State == "needs-you" || s.State == "failed" || s.State == store.StateStopped || isStopped(s)
	done := s.State == "review" || s.State == "merged" || s.State == "closed"

	if s.State == "awaiting-answer" {
		items = append(items, paletteItem{"answer", "Answer the questions", "reply, then the agent resumes"})
	}
	// Pause a live run: stop the agent but keep the worktree, dropping the ticket
	// to NEEDS YOU so you can take over by hand (then Resume / Open a PR).
	if active {
		items = append(items, paletteItem{"pause", "Pause (move to NEEDS YOU)", "stop the running agent but keep the worktree so you can take over"})
	}
	if hasWT {
		if stuck {
			items = append(items,
				paletteItem{"resume", "Resume from last stage", "re-run verification on your fix, then open the PR"},
				paletteItem{"ship", "Open a PR", "skip verification - commit + push + PR (when the gate itself is unreliable here)"},
			)
		}
		items = append(items, paletteItem{"studio", "Open Android Studio", "open the worktree as a project"})
		// "Open in Claude Code" resumes the agent's session. Only offer it when no
		// run is active - otherwise the headless agent and this interactive session
		// would both write to the same session on disk and diverge.
		if !active {
			items = append(items, paletteItem{"claude", "Open in Claude Code", "resume the agent's session in a new terminal"})
		}
	} else if stuck || done {
		// Worktree is gone — only a fresh run is possible.
		items = append(items, paletteItem{"rerun", "Run again (fresh)", "no worktree left - start this ticket from scratch"})
	}
	// Stop is available for live/stuck rows (kills the process, removes the
	// worktree, and clears it into STOPPED) - but not for finished or already-
	// stopped rows.
	if !done && s.State != store.StateStopped {
		items = append(items, paletteItem{"stop", "Stop & clean up", "kill the process and remove the worktree"})
	}
	return items
}

// claudeClosedMsg is returned after launching a Claude Code terminal.
type claudeClosedMsg struct{ err error }

// interactiveOverride lifts the headless run's "do not push / do not open a PR"
// restrictions when the user resumes the session interactively. It is passed via
// claude's --append-system-prompt so it modifies NO file in the worktree - a
// CLAUDE.md edit would be picked up by the chained `run --resume` commit
// (git add -A) and pollute the user's PR.
const interactiveOverride = "You are now in an interactive session with the user. " +
	"Any earlier headless-mode restrictions (such as not pushing or not opening a pull request) are lifted. " +
	"Follow the user's instructions directly, including git push or opening a PR if they ask."

// openClaude opens an interactive Claude Code session in the ticket's worktree in
// a new Terminal window. When the session has a stored ID the history is resumed
// so the user can review what the agent did; an --append-system-prompt override
// lifts the headless-mode restrictions. The dashboard keeps running.
func (m monitorModel) openClaude(s store.Session) tea.Cmd {
	worktree := paths.WorktreeFor(s.Repo, s.Ticket)
	cmdline := "cd " + shellQuote(worktree) + " && claude"
	if s.SessionID != "" {
		cmdline += " --resume " + shellQuote(s.SessionID) +
			" --append-system-prompt " + shellQuote(interactiveOverride)
	}
	// When the ticket is stuck (needs-you/failed/stopped) the user opens this
	// session to fix it by hand. Chain magneton's own gate+PR after the
	// interactive session exits, so finishing the fix automatically re-runs
	// verification and opens the PR - no need to come back and tap "Create PR
	// from my fix". `run --resume` writes to the store, so the dashboard reflects
	// the outcome. (If the user closes the window instead of exiting claude the
	// chained command can't run - an inherent limit of doing this in-terminal.)
	stuck := s.State == "needs-you" || s.State == "failed" || s.State == store.StateStopped || isStopped(s)
	if self, err := os.Executable(); err == nil && stuck {
		cmdline += "; " + shellQuote(self) + " run " + shellQuote(s.Ticket) + " --resume"
	}
	// Window title: "TICKET-1 · ai/ticket-1-branch" (or just the ticket when no branch yet).
	title := s.Ticket
	if s.Branch != "" {
		title += " · " + s.Branch
	}
	// `do script` with no target always opens a NEW Terminal window and runs the
	// command there. Simple and reliable.
	safeCmd := asEscapeAS(cmdline)
	safeTitle := asEscapeAS(title)
	script := "tell application \"Terminal\"\n" +
		"\tactivate\n" +
		"\tset t to do script \"" + safeCmd + "\"\n" +
		"\ttry\n" +
		"\t\tset custom title of t to \"" + safeTitle + "\"\n" +
		"\tend try\n" +
		"end tell"
	return func() tea.Msg {
		// Run (not Start) so an AppleScript failure actually surfaces instead of
		// silently "succeeding". osascript returns as soon as the window is open;
		// it does not wait for the claude session.
		out, err := exec.Command("osascript", "-e", script).CombinedOutput()
		if err != nil {
			return claudeClosedMsg{err: fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))}
		}
		return claudeClosedMsg{}
	}
}

// asEscapeAS escapes a string for embedding inside an AppleScript double-quoted literal.
func asEscapeAS(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// doAction runs an action by id. The Enter menu and the keyboard shortcuts both
// route here so they stay in sync.
func (m monitorModel) doAction(id string) (tea.Model, tea.Cmd) {
	switch id {
	// --- per-agent ---
	case "answer":
		if s := m.selected(); s != nil && s.State == "awaiting-answer" {
			m.answering = true
			m.answerKey = s.Ticket
			m.answerAtoms = nil
			m.answerCursor = 0
			m.notice = ""
		}
	case "studio":
		if s := m.selected(); s != nil {
			wt := projectDirIn(paths.WorktreeFor(s.Repo, s.Ticket), s.Repo)
			// Prefer the JetBrains `studio` launcher (opens the dir as a project);
			// fall back to the macOS app.
			if _, err := exec.LookPath("studio"); err == nil {
				_ = exec.Command("studio", wt).Start()
			} else {
				_ = exec.Command("open", "-a", "Android Studio", wt).Start()
			}
			m.notice = "opening Android Studio in the worktree…"
		}
	case "claude":
		if s := m.selected(); s != nil {
			return m, m.openClaude(*s)
		}
	case "resume":
		if s := m.selected(); s != nil {
			m.notice = "resuming " + s.Ticket + " from the last stage…"
			arg := s.Ticket
			if s.SourcePath != "" {
				arg = s.SourcePath
			}
			return m, m.spawnRun(arg, "--resume")
		}
	case "ship":
		if s := m.selected(); s != nil {
			m.notice = "opening a PR for " + s.Ticket + " without re-verifying…"
			arg := s.Ticket
			if s.SourcePath != "" {
				arg = s.SourcePath
			}
			return m, m.spawnRun(arg, "--ship")
		}
	case "rerun":
		if s := m.selected(); s != nil {
			m.notice = "starting " + s.Ticket + " fresh…"
			arg := s.Ticket
			if s.SourcePath != "" {
				arg = s.SourcePath
			}
			return m, m.spawnRun(arg)
		}
	case "pause":
		if s := m.selected(); s != nil {
			m.notice = "pausing " + s.Ticket + " - moving to NEEDS YOU…"
			return m, m.pauseAgent(*s)
		}
	case "stop":
		if s := m.selected(); s != nil {
			m.confirming = s.Ticket
			m.confirmCursor = 0
			m.notice = ""
		}
	// --- global ---
	case "run":
		m.runMode = ""
		m.runMethodCursor = 0
		m.runText = ""
		m.runTickets = nil
		m.runIDPrompt = -1
		m.runImgPrompt = -1
		m.notice = ""
		// With a single input method (no Jira configured) the picker is just an
		// extra keystroke - jump straight into that method's input.
		if methods := m.runMethods(); len(methods) == 1 {
			m.runMode = methods[0].mode
			m.view = viewRunInput
		} else {
			m.view = viewRunMethod
		}
	case "doctor":
		m.notice = "running doctor…"
		return m, m.runDoctor()
	case "config":
		m.openConfigForm()
	case "setup":
		m.openSetupForm()
	case "daemon-start":
		m.notice = "starting daemon…"
		return m, m.startDaemon()
	case "daemon-stop":
		m.notice = "stopping daemon…"
		return m, m.stopDaemon()
	case "menu":
		m.view = viewPalette
		m.paletteCursor = 0
		m.notice = ""
	case "refresh":
		m.reload()
	case "quit":
		return m, tea.Quit
	// --- confirmations ---
	case "confirm-yes":
		return m.updateConfirming(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	case "confirm-no":
		m.confirming = ""
	}
	return m, nil
}
