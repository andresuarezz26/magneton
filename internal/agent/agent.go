// Package agent drives a headless Claude Code session (Decision 3) and reads the
// machine-readable completion contract it writes (Decision 6).
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Report is the .agent/report.json the session must write as its last step.
type Report struct {
	Status       string   `json:"status"` // "ready_for_build" | "needs_human"
	Summary      string   `json:"summary"`
	FilesChanged []string `json:"filesChanged"`
	Branch       string   `json:"branch"`
	Tests        string   `json:"tests"`
	// PRBody is the repo's own pull-request template, filled in for this change.
	// Empty when the repo has no template (the orchestrator then falls back to
	// magneton's default PR body).
	PRBody string `json:"prBody,omitempty"`
	// Verified is the agent's self-certification from the verify stage: true once
	// it has itself run this project's build + tests and seen them pass. nil/false
	// means unverified - the orchestrator stops at needs-you instead of opening a
	// PR. magneton trusts this flag rather than running its own Gradle commands, so
	// per-project build setups and company-managed build skills all work.
	Verified *bool `json:"verified,omitempty"`
	// VerifyLog is a human-readable note of what the verify stage ran and the
	// outcome (and, on failure, the failing output tail and why).
	VerifyLog string `json:"verifyLog,omitempty"`
}

// Plan is the .agent/plan.json the plan stage must write before implementation.
// It no longer records build/test commands - the verify stage discovers and runs
// verification itself. NeedsEmulator is kept so the orchestrator can coordinate
// the shared emulator across concurrent tickets before the verify stage runs.
type Plan struct {
	Plan          string   `json:"plan"`           // the full plan, free-form markdown - Claude structures it however it wants
	Steps         []string `json:"steps"`          // legacy: no longer requested; still injected/rendered if a plan carries them
	Questions     []string `json:"questions"`      // blocking ambiguities; empty = proceed
	Confidence    string   `json:"confidence"`     // "high" | "medium" | "low"
	Type          string   `json:"type"`           // "bug" | "feature" | "chore"
	NeedsEmulator bool     `json:"needs_emulator"` // true if task requires instrumentation tests
	Diagram       string   `json:"diagram"`        // legacy: no longer requested; still rendered if a plan carries one
}

// Review is the .agent/review.json the self-review stage must write.
type Review struct {
	Verdict string   `json:"verdict"` // "pass" | "fix"
	Issues  []string `json:"issues"`  // actionable fix items when verdict=="fix"
}

// PlanMarkdown renders a plan as a markdown document (shared by the TUI's plan
// viewer and the durable ~/.magneton/plans/<ticket>.md archive).
func PlanMarkdown(title string, plan *Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	if plan.Confidence != "" || plan.Type != "" {
		fmt.Fprintf(&b, "**Confidence:** %s  ·  **Type:** %s\n\n", plan.Confidence, plan.Type)
	}
	// The plan body is free-form markdown with its own structure - include it
	// verbatim rather than forcing it under a heading.
	if strings.TrimSpace(plan.Plan) != "" {
		fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(plan.Plan))
	}
	if strings.TrimSpace(plan.Diagram) != "" {
		fmt.Fprintf(&b, "## Diagram\n\n%s\n\n", plan.Diagram)
	}
	if len(plan.Steps) > 0 {
		b.WriteString("## Steps\n\n")
		for i, step := range plan.Steps {
			fmt.Fprintf(&b, "%d. %s\n", i+1, step)
		}
		b.WriteString("\n")
	}
	if len(plan.Questions) > 0 {
		b.WriteString("## Open questions\n\n")
		for _, q := range plan.Questions {
			fmt.Fprintf(&b, "- %s\n", q)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// PlanTools is the read-only allowlist for the plan stage.
// Write is included only to create .agent/plan.json; source files must not be
// edited. Task (subagent exploration) and TodoWrite (Claude Code's own plan
// tracking) are allowed so the model can plan with its native tooling - both
// are read-only with respect to the source tree.
const PlanTools = "Write Read Glob Grep Task TodoWrite " +
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
	SettingsJSON string // inline --settings payload (e.g. sandbox posture); empty = omit
	Logf         func(format string, args ...interface{})
}

// buildArgs assembles the `claude` argv for the given prompt and options. Split
// out from Run so the flag wiring (notably --settings) is unit-testable without
// spawning the CLI.
func buildArgs(prompt string, o Options) []string {
	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "acceptEdits",
		"--allowed-tools", o.AllowedTools,
	}
	if o.SettingsJSON != "" {
		args = append(args, "--settings", o.SettingsJSON)
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
	return args
}

// Run invokes `claude -p` with stream-json, streaming progress through Logf.
// It returns the session id (for resuming on retry) and any process error.
func Run(prompt string, o Options) (sessionID string, err error) {
	cmd := exec.Command("claude", buildArgs(prompt, o)...)
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
	// Read the stream in a goroutine and wait on the process separately. The
	// agent can launch background processes during verify (e.g. `./gradlew … &`,
	// and Gradle in turn forks a long-lived daemon) that inherit claude's stdout
	// fd. When that happens the pipe never reaches EOF even after claude exits,
	// so reading to EOF *before* Wait() would hang forever - which left tickets
	// stuck in the build/running state after the agent had already finished.
	// cmd.Wait() returns when the claude process itself exits (it only reaps the
	// direct child, not the reparented daemon) and closes the read pipe, which
	// unblocks parseStream.
	idCh := make(chan string, 1)
	go func() { idCh <- parseStream(stdout, o.Logf) }()
	err = cmd.Wait()
	sessionID = <-idCh
	return sessionID, err
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
				model, _ := ev["model"].(string)
				if model != "" {
					logf("  • session %s started · model:%s", short(sessionID), model)
				} else {
					logf("  • session %s started", short(sessionID))
				}
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

// readAgentFile reads .agent/<name> from the worktree. The contract is that the
// agent writes its scratch dir at the worktree root, but when the configured
// repo is a module inside a larger repo, `git worktree add` checks out the whole
// containing repo and the actual project (where the agent works and writes
// .agent/) is a SUBDIRECTORY of the worktree. So if the file isn't at the root
// we search the tree for the shallowest .agent/<name> and use that.
func readAgentFile(worktreeDir, name string) ([]byte, error) {
	root := filepath.Join(worktreeDir, ".agent", name)
	if b, err := os.ReadFile(root); err == nil {
		return b, nil
	}
	if found := findAgentFile(worktreeDir, name); found != "" {
		if b, err := os.ReadFile(found); err == nil {
			return b, nil
		}
	}
	// Return the root-path error so the message names the expected location.
	return os.ReadFile(root)
}

// findAgentFile returns the shallowest path to a "<dir>/.agent/<name>" anywhere
// under worktreeDir, or "" if none. Heavy/irrelevant dirs are skipped to keep the
// walk cheap; this only runs on the fallback path (file not at the root).
func findAgentFile(worktreeDir, name string) string {
	best, bestDepth := "", 1<<30
	_ = filepath.WalkDir(worktreeDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		switch d.Name() {
		case ".git", "build", ".gradle", "node_modules", ".idea":
			return filepath.SkipDir
		}
		cand := filepath.Join(path, ".agent", name)
		if _, statErr := os.Stat(cand); statErr == nil {
			if depth := strings.Count(path, string(os.PathSeparator)); depth < bestDepth {
				best, bestDepth = cand, depth
			}
		}
		return nil
	})
	return best
}

// ReadReport loads .agent/report.json from the worktree.
func ReadReport(worktreeDir string) (*Report, error) {
	b, err := readAgentFile(worktreeDir, "report.json")
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
	b, err := readAgentFile(worktreeDir, "plan.json")
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
	b, err := readAgentFile(worktreeDir, "review.json")
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
	return fmt.Sprintf(`Plan the implementation of a ticket in this Android codebase.
Understand the code and produce your implementation plan however you see fit,
using whatever tools and approach work best for you. Do NOT edit any source
files - this is a planning step only.

TICKET %s: %s

%s

When your plan is ready, write it to .agent/plan.json (create the .agent directory if needed):
{
  "plan": "<your full implementation plan, as markdown - any length and structure you consider right>",
  "questions": ["<question if truly blocking>"],
  "confidence": "high" | "medium" | "low",
  "type": "bug" | "feature" | "chore",
  "needs_emulator": true | false
}
The fields besides "plan" drive the orchestrator:
- questions: only genuine ambiguities that block safe implementation - prefer a
  reasonable assumption over a question. Empty array = proceed without human input.
  If the human left "Plan feedback" in the ticket text, treat it as the priority:
  revise the plan to address it directly.
- needs_emulator coordinates a shared Android emulator across tickets. Set it true
  if the work involves UI/Espresso/Compose instrumentation tests, files under
  androidTest/, or the ticket mentions connected tests; false for domain logic,
  ViewModels, repositories, unit tests under test/, or pure Kotlin/Java changes.`,
		ticketKey, summary, desc)
}

// BuildImplPrompt is the instruction for the implement stage, injecting the approved plan.
func BuildImplPrompt(ticketKey, summary, description string, plan *Plan) string {
	desc := strings.TrimSpace(description)
	if desc == "" {
		desc = "(no description provided)"
	}
	// The plan is free-form markdown. Legacy plans (pre-2.0 archives) may carry a
	// separate steps list - append it so nothing is lost on re-runs.
	planBlock := plan.Plan
	if len(plan.Steps) > 0 {
		planBlock += "\n\nIMPLEMENTATION STEPS:\n" + strings.Join(plan.Steps, "\n")
	}
	return fmt.Sprintf(`You are an autonomous Android engineer implementing a pre-approved plan inside an isolated git worktree (your current working directory).

TICKET %s: %s

%s

APPROVED PLAN:
%s

Rules:
- Follow the approved plan above. Make the focused, minimal change described.
- This is an Android/Gradle project. A later verification step will build the project and run its tests - write code that compiles and passes.
- Do NOT git push and do NOT open a pull request - the orchestrator handles commit, push, and PR.
- PR description: check whether this repo has a pull request template (.github/PULL_REQUEST_TEMPLATE.md, .github/pull_request_template.md, or docs/PULL_REQUEST_TEMPLATE.md). If one exists, fill it out for THIS change: EVERY heading, section, and checklist item MUST appear in prBody, in the same order. Never drop a section - fill each one with real content about THIS change. Keep every checklist item exactly as written: tick the boxes that apply, leave the rest unticked. Do not add sections, filler text, or commentary not in the template. Put the finished markdown in "prBody". If there is NO template, set "prBody" to "".
- Your FINAL action MUST be to write .agent/report.json (create the .agent directory if needed):
{
  "status": "ready_for_build" | "needs_human",
  "summary": "<one line: what changed and why>",
  "filesChanged": ["<relative paths>"],
  "branch": "",
  "tests": "<what you checked, if anything>",
  "prBody": "<repo PR template filled in, or \"\" if the repo has none>"
}
Use "needs_human" if you hit an unexpected blocker you cannot resolve safely.`,
		ticketKey, summary, desc, planBlock)
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
3. Be an adversarial reviewer - only pass if the change is genuinely good.

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

// BuildVerifyPrompt has the session discover and RUN this project's own build +
// test verification, then record the verdict in report.json. The agent
// self-certifies - magneton trusts report.verified rather than running hardcoded
// Gradle commands, so per-project build setups and company-managed build skills
// all work, and the agent's process uses the real environment (no isolated-Gradle
// TLS/cert problems). The agent discovers the build/test commands itself. When
// allowFix is false it only confirms a human's existing fix and must not edit
// code (the resume path). emulator tells it whether instrumented tests can run.
func BuildVerifyPrompt(ticketKey, summary string, emulator, allowFix bool) string {
	device := "No emulator/device is attached - run UNIT tests only; do NOT run instrumented/connected androidTest tasks."
	if emulator {
		device = "An Android emulator is booted and attached via adb - also run the instrumented/connected tests."
	}
	fixRule := `4. If the build or any test fails, FIX the code and re-run until everything passes. Set "verified": true ONLY after you have actually seen the build AND tests pass.`
	if !allowFix {
		fixRule = `4. Do NOT modify any source files - a human has already made the fix and you are only confirming it. Run the build + tests and report the result honestly.`
	}
	return fmt.Sprintf(`You are verifying that the change for ticket %s (%s) actually builds and passes its tests, inside an isolated git worktree (your current working directory).

magneton does NOT run the build for you - YOU discover and run it. Android/Gradle setups differ per project and some teams ship their own build/test scripts or skills, so figure out the right way to verify THIS repo:
1. Discover how this project builds and tests: inspect build.gradle(.kts), gradle.properties, settings.gradle, a Makefile, scripts/, README/CONTRIBUTING, CI config under .github/, and any company-provided build skill. No build/test commands are pre-configured - find them yourself.
2. Compile the project. %s
3. Run the tests. Capture the REAL pass/fail result - never assume it passed.
%s
- Do NOT git commit, git push, or open a pull request - the orchestrator owns commit/push/PR.
- Your FINAL action MUST be to update .agent/report.json: read the existing file and rewrite it preserving every existing field, adding/setting:
  "verified": true | false   (true ONLY if the build AND tests actually passed)
  "verifyLog": "<the commands you ran and their outcome; on failure include the failing output tail and why>"
Setting "verified": false is a normal outcome (not an error) when you cannot get it green - it routes the ticket to a human.`,
		ticketKey, summary, device, fixRule)
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
- This is an Android/Gradle project. The orchestrator will verify with compile %q and tests %q - write code that will pass them.
- Do NOT git push and do NOT open a pull request - the orchestrator handles build, commit, push, and PR.
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

// Oneshot runs `claude -p <prompt>` with a 30 s timeout and returns the first
// non-blank output line. It uses the haiku model for speed and low cost.
// Returns "" on any error — callers keep their programmatic fallback.
func Oneshot(prompt, anthropicKey string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude",
		"-p", prompt,
		"--model", "claude-haiku-4-5-20251001",
		"--output-format", "text",
	)
	cmd.Env = os.Environ()
	if anthropicKey != "" {
		cmd.Env = append(cmd.Env, "ANTHROPIC_API_KEY="+anthropicKey)
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
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
