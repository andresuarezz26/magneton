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

// Plan is the .agent/plan.json the plan stage must write before implementation.
type Plan struct {
	Plan       string   `json:"plan"`       // one-line approach summary
	Steps      []string `json:"steps"`      // ordered implementation steps
	Questions  []string `json:"questions"`  // blocking ambiguities; empty = proceed
	Confidence string   `json:"confidence"` // "high" | "medium" | "low"
	Type       string   `json:"type"`       // "bug" | "feature" | "chore"
	CompileCmd string   `json:"compileCmd"` // discovered Gradle compile command
	TestCmd    string   `json:"testCmd"`    // discovered Gradle test command
}

// Review is the .agent/review.json the self-review stage must write.
type Review struct {
	Verdict string   `json:"verdict"` // "pass" | "fix"
	Issues  []string `json:"issues"`  // actionable fix items when verdict=="fix"
}

// PlanTools is the read-only allowlist for the plan stage.
// Write is included only to create .agent/plan.json; source files must not be edited.
const PlanTools = "Write Read Glob Grep " +
	"Bash(ls:*) Bash(cat:*) Bash(head:*) Bash(tail:*) " +
	"Bash(grep:*) Bash(rg:*) Bash(find:*) Bash(git status:*) " +
	"Bash(git diff:*) Bash(git log:*) Bash(git show:*)"

// ReviewTools is the minimal allowlist for the self-review stage.
const ReviewTools = "Write Read Glob Grep " +
	"Bash(git diff:*) Bash(git show:*) Bash(git log:*) Bash(ls:*)"

// Options configure a single claude invocation.
type Options struct {
	WorktreeDir  string
	AllowedTools string
	MaxBudgetUSD float64
	AnthropicKey string
	Model        string // e.g. "claude-opus-4-8", "claude-sonnet-4-6"; empty = claude default
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
	if o.Model != "" {
		args = append(args, "--model", o.Model)
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
				logf("  %s", strings.TrimSpace(t))
			}
		case "tool_use":
			if name, _ := blk["name"].(string); name != "" {
				input, _ := blk["input"].(map[string]interface{})
				detail := toolDetail(name, input)
				if detail != "" {
					logf("  ⚙ %s(%s)", name, detail)
				} else {
					logf("  ⚙ %s", name)
				}
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

// ReadPlan loads .agent/plan.json from the worktree.
func ReadPlan(worktreeDir string) (*Plan, error) {
	p := filepath.Join(worktreeDir, ".agent", "plan.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var plan Plan
	if err := json.Unmarshal(b, &plan); err != nil {
		return nil, fmt.Errorf("parse plan.json: %w", err)
	}
	return &plan, nil
}

// ReadReview loads .agent/review.json from the worktree.
func ReadReview(worktreeDir string) (*Review, error) {
	p := filepath.Join(worktreeDir, ".agent", "review.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var rev Review
	if err := json.Unmarshal(b, &rev); err != nil {
		return nil, fmt.Errorf("parse review.json: %w", err)
	}
	return &rev, nil
}

// BuildPlanPrompt is the instruction for the plan stage (read-only, strong model).
func BuildPlanPrompt(ticketKey, summary, description string) string {
	desc := strings.TrimSpace(description)
	if desc == "" {
		desc = "(no description provided)"
	}
	return fmt.Sprintf(`You are a senior Android engineer planning the implementation of a Jira ticket.
Your job is to READ and UNDERSTAND the codebase, then produce a concrete plan.
Do NOT edit any source files — this is a planning step only.

TICKET %s: %s

%s

Instructions:
1. Explore the codebase (git log, find, grep, cat) to understand the affected code.
2. Identify the minimal, focused change needed to resolve this ticket.
3. List any genuine ambiguities that would block safe implementation (questions[]).
   Only list a question if you truly cannot make a safe assumption — prefer a reasonable default.
4. Classify the ticket type: "bug", "feature", or "chore".
5. Rate your confidence: "high" (clear path), "medium" (some uncertainty), "low" (significant unknowns).
6. Discover the correct Gradle commands for this project by inspecting build.gradle files.
   For compileCmd: find a fully-qualified task like "./gradlew :app:compileDebugSources" or "./gradlew assembleDebug".
     Run "./gradlew tasks --all 2>/dev/null | grep -i compile | head -20" to find valid tasks.
   For testCmd: find a fully-qualified task like "./gradlew :app:testDebugUnitTest".
     Run "./gradlew tasks --all 2>/dev/null | grep -i test | head -20" to find valid tasks.
   Use empty string if the project has no Gradle setup.

Your ONLY write action is to create .agent/plan.json (create the .agent directory if needed):
{
  "plan": "<one sentence: what will change and why>",
  "steps": ["<step 1>", "<step 2>", ...],
  "questions": ["<question if truly blocking>"],
  "confidence": "high" | "medium" | "low",
  "type": "bug" | "feature" | "chore",
  "compileCmd": "<fully-qualified gradlew compile command, or empty string>",
  "testCmd": "<fully-qualified gradlew test command, or empty string>"
}
Use an empty array for questions if you can proceed without human input.`,
		ticketKey, summary, desc)
}

// BuildImplPrompt is the instruction for the implement stage, injecting the approved plan.
func BuildImplPrompt(ticketKey, summary, description string, plan *Plan, compileCmd, testCmd string) string {
	desc := strings.TrimSpace(description)
	if desc == "" {
		desc = "(no description provided)"
	}
	steps := strings.Join(plan.Steps, "\n")
	return fmt.Sprintf(`You are an autonomous Android engineer implementing a pre-approved plan inside an isolated git worktree (your current working directory).

TICKET %s: %s

%s

APPROVED PLAN: %s

IMPLEMENTATION STEPS:
%s

Rules:
- Follow the approved plan and steps above. Make the focused, minimal change described.
- This is an Android/Gradle project. The orchestrator will verify with compile %q and tests %q — write code that will pass them.
- Do NOT git push and do NOT open a pull request — the orchestrator handles build, commit, push, and PR.
- Your FINAL action MUST be to write .agent/report.json (create the .agent directory if needed):
{
  "status": "ready_for_build" | "needs_human",
  "summary": "<one line: what changed and why>",
  "filesChanged": ["<relative paths>"],
  "branch": "",
  "tests": "<what you checked, if anything>"
}
Use "needs_human" if you hit an unexpected blocker you cannot resolve safely.`,
		ticketKey, summary, desc, plan.Plan, steps, compileCmd, testCmd)
}

// BuildReviewPrompt is the instruction for the self-review stage.
func BuildReviewPrompt(ticketKey, summary string, plan *Plan) string {
	return fmt.Sprintf(`You are a senior Android code reviewer adversarially reviewing changes made for ticket %s: %s

The approved plan was: %s

Instructions:
1. Run "git diff HEAD" to see all changes made in this worktree.
2. Review for:
   - Correctness: does the change actually solve the ticket as planned?
   - Scope: did the agent change more than the plan described?
   - Android conventions: proper Kotlin idioms, no deprecated APIs, correct threading?
   - Safety: nullability issues, resource leaks, or crash risks?
3. Be an adversarial reviewer — only pass if the change is genuinely good.

Your ONLY write action is to create .agent/review.json:
{
  "verdict": "pass" | "fix",
  "issues": ["<specific, actionable issue>"]
}
Use "pass" if the change correctly implements the plan with no significant issues.
Use "fix" with a list of specific problems that must be corrected.`,
		ticketKey, summary, plan.Plan)
}

// BuildReviewFixPrompt feeds self-review issues back into the implement session.
func BuildReviewFixPrompt(issues []string) string {
	list := strings.Join(issues, "\n- ")
	return fmt.Sprintf(`A self-review found the following issues with your implementation. Fix each one, then rewrite .agent/report.json as your final step.

Issues to fix:
- %s`, list)
}

// BuildPrompt is the legacy single-stage instruction (kept for tests and --local fallback).
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

// toolDetail extracts the most useful display string from a tool's input map.
func toolDetail(name string, input map[string]interface{}) string {
	if input == nil {
		return ""
	}
	switch name {
	case "Read":
		if p, _ := input["file_path"].(string); p != "" {
			return p
		}
	case "Write":
		if p, _ := input["file_path"].(string); p != "" {
			return p
		}
	case "Edit", "MultiEdit":
		if p, _ := input["file_path"].(string); p != "" {
			return p
		}
	case "Bash":
		if c, _ := input["command"].(string); c != "" {
			return oneLine(c, 80)
		}
	case "Glob":
		if p, _ := input["pattern"].(string); p != "" {
			return p
		}
	case "Grep":
		if p, _ := input["pattern"].(string); p != "" {
			return p
		}
	case "TodoWrite":
		return ""
	}
	// Fallback: first string value found.
	for _, v := range input {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return oneLine(s, 60)
		}
	}
	return ""
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
