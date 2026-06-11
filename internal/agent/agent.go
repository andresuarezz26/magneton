// Package agent drives a headless Claude Code session (Decision 3) and reads the
// machine-readable completion contract it writes (Decision 6).
package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Report is the .agent/report.json the session must write as its last step.
type Report struct {
	Status       string   `json:"status"` // "ready_for_build" | "needs_human"
	Summary      string   `json:"summary"`
	FilesChanged []string `json:"filesChanged"`
	Branch       string   `json:"branch"`
	Tests        string   `json:"tests"`
}

// Options configure a single claude invocation.
type Options struct {
	WorktreeDir  string
	AllowedTools string
	MaxBudgetUSD float64
	AnthropicKey string
	ResumeID     string // set to resume the same session for retries (Decision 4)
	Logf         func(format string, args ...interface{})
}

// Run invokes `claude -p` with stream-json, streaming progress through Logf.
// It returns the session id (for resuming on retry) and any process error.
func Run(prompt string, o Options) (sessionID string, err error) {
	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "acceptEdits",
		"--allowed-tools", o.AllowedTools,
	}
	if o.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%g", o.MaxBudgetUSD))
	}
	if o.ResumeID != "" {
		args = append(args, "--resume", o.ResumeID)
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = o.WorktreeDir
	cmd.Env = os.Environ()
	if o.AnthropicKey != "" {
		cmd.Env = append(cmd.Env, "ANTHROPIC_API_KEY="+o.AnthropicKey)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	sessionID = parseStream(stdout, o.Logf)
	return sessionID, cmd.Wait()
}

func parseStream(r io.Reader, logf func(string, ...interface{})) string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	var sessionID string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev map[string]interface{}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if id, ok := ev["session_id"].(string); ok && id != "" {
			sessionID = id
		}
		switch ev["type"] {
		case "system":
			if ev["subtype"] == "init" {
				logf("  • session %s started", short(sessionID))
			}
		case "assistant":
			logAssistant(ev, logf)
		case "result":
			if s, ok := ev["result"].(string); ok && strings.TrimSpace(s) != "" {
				logf("  • %s", oneLine(s, 200))
			}
		}
	}
	return sessionID
}

func logAssistant(ev map[string]interface{}, logf func(string, ...interface{})) {
	msg, _ := ev["message"].(map[string]interface{})
	if msg == nil {
		return
	}
	content, _ := msg["content"].([]interface{})
	for _, c := range content {
		blk, _ := c.(map[string]interface{})
		switch blk["type"] {
		case "text":
			if t, _ := blk["text"].(string); strings.TrimSpace(t) != "" {
				logf("  %s", oneLine(t, 160))
			}
		case "tool_use":
			if name, _ := blk["name"].(string); name != "" {
				logf("  ⚙ %s", name)
			}
		}
	}
}

// ReadReport loads .agent/report.json from the worktree.
func ReadReport(worktreeDir string) (*Report, error) {
	p := filepath.Join(worktreeDir, ".agent", "report.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse report.json: %w", err)
	}
	return &r, nil
}

// BuildPrompt is the initial instruction for a ticket.
func BuildPrompt(ticketKey, summary, description, compileCmd, testCmd string) string {
	desc := strings.TrimSpace(description)
	if desc == "" {
		desc = "(no description provided)"
	}
	return fmt.Sprintf(`You are an autonomous Android engineer resolving a single Jira ticket inside an isolated git worktree (your current working directory).

TICKET %s: %s

%s

Rules:
- Make the focused, minimal code change that resolves this ticket. Stay within this worktree.
- This is an Android/Gradle project. The orchestrator will verify with compile %q and tests %q — write code that will pass them.
- Do NOT git push and do NOT open a pull request — the orchestrator handles build, commit, push, and PR.
- Your FINAL action MUST be to write .agent/report.json (create the .agent directory if needed) with exactly:
{
  "status": "ready_for_build" | "needs_human",
  "summary": "<one line: what changed and why>",
  "filesChanged": ["<relative paths>"],
  "branch": "",
  "tests": "<what you checked, if anything>"
}
Use "needs_human" if the ticket is ambiguous, out of scope, or you cannot make a safe change; otherwise "ready_for_build".`,
		ticketKey, summary, desc, compileCmd, testCmd)
}

// BuildRetryPrompt feeds a failed gate back into the same session (Decision 4).
func BuildRetryPrompt(phase, output string) string {
	return fmt.Sprintf(`The %s step FAILED. Fix the code so it passes, then rewrite .agent/report.json as your final step.

--- %s output (tail) ---
%s`, phase, phase, tail(output, 6000))
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func oneLine(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
