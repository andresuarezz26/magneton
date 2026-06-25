// Package runner is the per-ticket pipeline: worktree → agent → gate → PR.
// It is driven identically by the CLI (`agent run`) and the daemon; callers
// observe progress through Hooks (Decision 9 lifecycle states).
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/andresuarezz26/magneton/internal/agent"
	"github.com/andresuarezz26/magneton/internal/build"
	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/git"
	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/secrets"
	"github.com/andresuarezz26/magneton/internal/store"
	"github.com/andresuarezz26/magneton/internal/vcs"
)

// Task is one ticket to process.
type Task struct {
	Ticket      string
	Summary     string
	Description string
	Repo        *config.Repo
	Cfg         *config.Config
	DryRun      bool
	Resume      bool         // verify & ship: continue from the existing worktree, no re-plan/implement
	Store       *store.Store // optional; enables emulator coordination
}

// Hooks let the caller observe and react. Any field may be nil.
type Hooks struct {
	Logf    func(string, ...interface{})      // progress lines
	OnState func(state string, retries int)   // lifecycle transitions
	OnField func(branch, worktree, pr string) // metadata as it's known
	Comment func(text string)                 // post back to the ticket
}

// Outcome is the terminal result.
type Outcome struct {
	State string // review | needs-you | failed
	PRURL string
	Err   error
}

// Run executes the full staged pipeline for one ticket:
// PLAN → CLARIFY → IMPLEMENT → SELF-REVIEW → GATE → PR
func Run(t Task, h Hooks) Outcome {
	if t.Resume {
		return resumeShip(t, h)
	}
	logf := h.Logf
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}
	setState := func(s string, r int) {
		if h.OnState != nil {
			h.OnState(s, r)
		}
	}
	needsYouComment := func(msg string) {
		if h.Comment != nil {
			h.Comment(fmt.Sprintf("🤖 *magneton needs your help on [%s]*\n\n%s\n\nCheck the session log: `agent logs %s`",
				t.Ticket, msg, t.Ticket))
		}
	}

	repo := t.Repo
	branch := strings.NewReplacer(
		"{ticket}", strings.ToLower(t.Ticket),
		"{slug}", slugify(t.Summary),
	).Replace(repo.Branch)
	worktree := paths.WorktreeFor(t.Ticket)
	gradleHome := paths.GradleHomeFor(t.Ticket)
	anthropicKey := secrets.Get(secrets.Anthropic)

	// 1. Provision an isolated worktree (Decision 7).
	setState(store.StatePlanning, 0)
	logf("[%s] worktree %s on %s", t.Ticket, worktree, branch)
	if err := git.CreateWorktree(repo.Path, worktree, branch, repo.Base); err != nil {
		setState(store.StateFailed, 0)
		return Outcome{State: store.StateFailed, Err: fmt.Errorf("create worktree: %w", err)}
	}
	if h.OnField != nil {
		h.OnField(branch, worktree, "")
	}

	// Write local.properties so Gradle can find the Android SDK in a fresh worktree.
	if err := paths.WriteLocalProperties(worktree, t.Cfg.AndroidSDKPath); err != nil {
		logf("[%s] (warn) local.properties: %v", t.Ticket, err)
	}

	// 2. PLAN stage — strong model, read-only tools.
	modelPlan := t.Cfg.ModelPlan
	modelImpl := t.Cfg.ModelImpl
	modelReview := t.Cfg.ModelReview
	logf("[%s] stage:plan model:%s", t.Ticket, modelPlan)
	planOpts := agent.Options{
		WorktreeDir:  worktree,
		AllowedTools: agent.PlanTools,
		Model:        modelPlan,
		MaxBudgetUSD: t.Cfg.MaxBudgetUSD,
		AnthropicKey: anthropicKey,
		Logf:         logf,
	}
	if _, err := agent.Run(agent.BuildPlanPrompt(t.Ticket, t.Summary, t.Description), planOpts); err != nil {
		logf("[%s] (warn) plan stage exited: %v", t.Ticket, err)
	}

	plan, err := agent.ReadPlan(worktree)
	if err != nil {
		logf("[%s] needs-you: plan stage did not produce plan.json (%v)", t.Ticket, err)
		setState(store.StateNeedsYou, 0)
		needsYouComment(fmt.Sprintf("The plan stage failed to produce a plan — the ticket may be too ambiguous or the codebase too complex to analyse automatically.\n\nError: `%v`", err))
		return Outcome{State: store.StateNeedsYou}
	}
	logf("[%s] plan (%s/%s): %s", t.Ticket, plan.Type, plan.Confidence, oneLine(plan.Plan, 120))

	// Resolve compile/test commands: config wins if set, otherwise use plan's discovered commands.
	compileCmd := repo.Compile
	if compileCmd == "" {
		compileCmd = plan.CompileCmd
	}
	testCmd := repo.Test
	if testCmd == "" {
		testCmd = plan.TestCmd
	}
	if compileCmd != "" {
		logf("[%s] compile: %s", t.Ticket, compileCmd)
	}
	if testCmd != "" {
		logf("[%s] test:    %s", t.Ticket, testCmd)
	}

	// Start emulator in the background, concurrent with the implement stage.
	// We do this right after reading the plan so boot time overlaps with Claude.
	needsEmu := plan.NeedsEmulator && t.Cfg.AVDName != "" && t.Store != nil
	sdkPaths := build.ResolvePaths(t.Cfg.AndroidSDKPath)
	var emulatorReady chan error
	if needsEmu {
		_ = t.Store.RegisterEmulator(t.Cfg.AVDName)
		emulatorReady = make(chan error, 1)
		go bootOrWait(t.Cfg.AVDName, sdkPaths, t.Store, logf, emulatorReady)
		logf("[%s] emulator boot started in background (avd: %s, adb: %s)", t.Ticket, t.Cfg.AVDName, sdkPaths.ADB)
	}

	// 3. CLARIFY — always post plan to Jira; stop if there are blocking questions.
	if h.Comment != nil {
		if comment, cerr := vcs.RenderPlanComment(vcs.PlanData{
			Ticket: t.Ticket, Summary: t.Summary,
			Plan: plan.Plan, Steps: plan.Steps, Questions: plan.Questions,
			Confidence: plan.Confidence, Type: plan.Type,
		}); cerr == nil {
			h.Comment(comment)
		}
	}
	if len(plan.Questions) > 0 {
		logf("[%s] awaiting-answer: %d question(s) posted to ticket", t.Ticket, len(plan.Questions))
		for i, q := range plan.Questions {
			logf("[%s]   Q%d: %s", t.Ticket, i+1, q)
		}
		setState(store.StateAwaiting, 0)
		return Outcome{State: store.StateAwaiting}
	}

	// 4. IMPLEMENT stage — fast model, full tools, plan injected.
	setState(store.StateWorking, 0)
	logf("[%s] stage:implement model:%s", t.Ticket, modelImpl)
	implOpts := agent.Options{
		WorktreeDir:  worktree,
		AllowedTools: t.Cfg.AllowedTools,
		Model:        modelImpl,
		MaxBudgetUSD: t.Cfg.MaxBudgetUSD,
		AnthropicKey: anthropicKey,
		Logf:         logf,
	}
	sessionID, runErr := agent.Run(
		agent.BuildImplPrompt(t.Ticket, t.Summary, t.Description, plan, compileCmd, testCmd),
		implOpts)
	if runErr != nil {
		logf("[%s] (warn) implement stage exited: %v", t.Ticket, runErr)
	}
	// Persist the Claude session id so "Open in Claude Code" can `claude --resume`
	// straight back into the agent's conversation for this ticket.
	saveSession := func(id string) {
		if t.Store != nil && id != "" {
			_ = t.Store.SetSessionID(t.Ticket, id)
		}
	}
	saveSession(sessionID)

	report, err := agent.ReadReport(worktree)
	if err != nil {
		logf("[%s] needs-you: missing/invalid report.json (%v)", t.Ticket, err)
		setState(store.StateNeedsYou, 0)
		needsYouComment("The implement stage ended without producing a completion report — the session likely crashed or timed out mid-run.")
		return Outcome{State: store.StateNeedsYou}
	}
	if report.Status == "needs_human" {
		logf("[%s] needs-you: agent reported needs_human — %s", t.Ticket, report.Summary)
		setState(store.StateNeedsYou, 0)
		needsYouComment(fmt.Sprintf("The agent determined it cannot safely complete this ticket automatically:\n\n> %s", report.Summary))
		return Outcome{State: store.StateNeedsYou}
	}
	logf("[%s] implement done: %s", t.Ticket, report.Summary)

	// 5. SELF-REVIEW — adversarial diff review; one fix round if issues found.
	setState(store.StateReviewing, 0)
	logf("[%s] stage:review model:%s", t.Ticket, modelReview)
	reviewOpts := agent.Options{
		WorktreeDir:  worktree,
		AllowedTools: agent.ReviewTools,
		Model:        modelReview,
		MaxBudgetUSD: t.Cfg.MaxBudgetUSD * 0.3,
		AnthropicKey: anthropicKey,
		Logf:         logf,
	}
	if _, err := agent.Run(agent.BuildReviewPrompt(t.Ticket, t.Summary, plan), reviewOpts); err != nil {
		logf("[%s] (warn) review stage exited: %v", t.Ticket, err)
	}
	if review, err := agent.ReadReview(worktree); err != nil {
		logf("[%s] (warn) no review.json — skipping self-review gate", t.Ticket)
	} else if review.Verdict == "fix" && len(review.Issues) > 0 {
		logf("[%s] self-review: %d issue(s) — applying one fix round", t.Ticket, len(review.Issues))
		fixOpts := implOpts
		fixOpts.ResumeID = sessionID
		if sid, ferr := agent.Run(agent.BuildReviewFixPrompt(review.Issues), fixOpts); ferr != nil {
			logf("[%s] (warn) fix round exited: %v", t.Ticket, ferr)
		} else if sid != "" {
			sessionID = sid
			saveSession(sessionID)
		}
	} else {
		logf("[%s] self-review: pass ✓", t.Ticket)
	}

	// 6. Build gate with bounded self-correct (Decision 4).
	// Sync with emulator and acquire it before running the gate.
	if needsEmu {
		logf("[%s] waiting for emulator…", t.Ticket)
		if err := <-emulatorReady; err != nil {
			logf("[%s] (warn) emulator unavailable: %v — falling back to unit tests", t.Ticket, err)
			needsEmu = false
		} else {
			for {
				ok, _ := t.Store.AcquireEmulator(t.Cfg.AVDName, t.Ticket)
				if ok {
					break
				}
				logf("[%s] emulator busy, waiting…", t.Ticket)
				time.Sleep(5 * time.Second)
			}
			defer func() { _ = t.Store.ReleaseEmulator(t.Cfg.AVDName) }()
			logf("[%s] emulator acquired", t.Ticket)
		}
	}

	activeTestCmd := testCmd
	if needsEmu && repo.ConnectedTest != "" {
		activeTestCmd = repo.ConnectedTest
	}
	logf("[%s] gate will run: compile=%q test=%q emulator=%v", t.Ticket, compileCmd, activeTestCmd, needsEmu)

	attempts := 1
	gate := func() build.Result {
		setState(store.StateBuilding, attempts-1)
		if r := build.Step(worktree, gradleHome, compileCmd, "compile"); !r.OK {
			return r
		}
		setState(store.StateTesting, attempts-1)
		return build.Step(worktree, gradleHome, activeTestCmd, "test")
	}
	for {
		res := gate()
		if res.OK {
			logf("[%s] gate green ✓", t.Ticket)
			break
		}
		logf("[%s] %s failed (attempt %d/%d)", t.Ticket, res.Phase, attempts, repo.MaxRetries)
		if attempts >= repo.MaxRetries {
			logf("[%s] needs-you: %s still red after %d attempts", t.Ticket, res.Phase, attempts)
			setState(store.StateNeedsYou, attempts-1)
			needsYouComment(fmt.Sprintf(
				"The *%s* step failed after %d attempt(s) and the agent could not fix it automatically.\n\n*Last error (tail):*\n{code}\n%s\n{code}\n\nOpen the worktree in Android Studio to investigate:\n`open -a \"Android Studio\" %s`",
				res.Phase, attempts, tail(res.Output, 1500), worktree,
			))
			return Outcome{State: store.StateNeedsYou}
		}
		attempts++
		implOpts.ResumeID = sessionID
		logf("[%s] feeding %s errors back to session", t.Ticket, res.Phase)
		if sid, err := agent.Run(agent.BuildRetryPrompt(res.Phase, res.Output), implOpts); err != nil {
			logf("[%s] (warn) claude retry exited: %v", t.Ticket, err)
		} else if sid != "" {
			sessionID = sid
			saveSession(sessionID)
		}
	}

	// 7-9. Commit, push, open PR (shared with the resume path).
	return finishShip(t, h, worktree, branch, attempts, report, logf, setState)
}

// finishShip commits any pending changes, and (unless dry-run) pushes the branch
// and opens a PR. Shared by the full pipeline and the resume path. report may be
// nil (resume has no fresh report.json).
func finishShip(t Task, h Hooks, worktree, branch string, attempts int, report *agent.Report,
	logf func(string, ...interface{}), setState func(string, int)) Outcome {
	repo := t.Repo

	// Re-read the freshest report (self-review/fix rounds rewrite it), archive it
	// in magneton's own home, and make sure the .agent/ scratch dir never lands in
	// the commit/PR. report may be nil on the resume path.
	if fresh, err := agent.ReadReport(worktree); err == nil {
		report = fresh
	}
	if report != nil {
		archiveReport(t.Ticket, report, logf)
	}
	git.UntrackAgentDir(worktree)

	if git.HasChanges(worktree) {
		if err := git.CommitAll(worktree, fmt.Sprintf("[%s] %s", t.Ticket, t.Summary)); err != nil {
			setState(store.StateFailed, attempts-1)
			return Outcome{State: store.StateFailed, Err: fmt.Errorf("commit: %w", err)}
		}
	}

	if t.DryRun {
		logf("[%s] dry-run: branch %s ready — skipping push + PR", t.Ticket, branch)
		setState(store.StateReview, attempts-1)
		return Outcome{State: store.StateReview}
	}

	base := repo.Base
	if base == "" {
		base = git.DefaultBranch(repo.Path)
	}
	setState(store.StateReview, attempts-1)
	logf("[%s] review · pushing %s", t.Ticket, branch)
	if err := git.Push(worktree, branch); err != nil {
		setState(store.StateFailed, attempts-1)
		return Outcome{State: store.StateFailed, Err: fmt.Errorf("push: %w", err)}
	}

	var files []string
	if report != nil {
		files = report.FilesChanged
	}
	pr := vcs.PRData{
		Ticket: t.Ticket, Summary: t.Summary, Branch: branch, Base: base,
		FilesChanged: files, Tests: "✅ compile + tests",
		Attempts: attempts, JiraBaseURL: t.Cfg.JiraBaseURL,
	}
	// Prefer the repo's own PR template (filled in by the agent); fall back to
	// magneton's default body only when the repo has no template.
	body := ""
	if report != nil && strings.TrimSpace(report.PRBody) != "" {
		body = report.PRBody
		logf("[%s] PR body from repo template", t.Ticket)
	} else {
		b, err := vcs.RenderPRBody(pr)
		if err != nil {
			return Outcome{State: store.StateFailed, Err: err}
		}
		body = b
	}
	prURL, err := vcs.OpenPR(worktree, base, fmt.Sprintf("[%s] %s", t.Ticket, t.Summary), body)
	if err != nil {
		setState(store.StateFailed, attempts-1)
		return Outcome{State: store.StateFailed, Err: err}
	}
	logf("[%s] PR opened: %s", t.Ticket, prURL)
	if h.OnField != nil {
		h.OnField("", "", prURL)
	}

	if h.Comment != nil {
		pr.PRURL = prURL
		if c, err := vcs.RenderJiraComment(pr); err == nil {
			h.Comment(c)
		}
	}
	logf("[%s] review — human-gated. magneton stops here.", t.Ticket)
	return Outcome{State: store.StateReview, PRURL: prURL}
}

// archiveReport persists a ticket's completion report into magneton's own home
// (~/.agent/reports/<ticket>.json) so it survives outside the worktree and can
// later be surfaced by a report viewer — without ever being committed to the
// target repo. Best-effort: a failure here never blocks shipping.
func archiveReport(ticket string, r *agent.Report, logf func(string, ...interface{})) {
	if err := os.MkdirAll(paths.Reports(), 0o755); err != nil {
		logf("[%s] (warn) could not create reports dir: %v", ticket, err)
		return
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(paths.ReportFor(ticket), b, 0o644); err != nil {
		logf("[%s] (warn) could not archive report: %v", ticket, err)
	}
}

// worktreeReady reports whether dir is a usable git worktree (has its .git link).
func worktreeReady(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// resumeShip continues a ticket from its EXISTING worktree, preserving manual
// changes: it re-runs the gate once and, if green, commits + pushes + opens a PR.
// It never re-plans or lets the agent edit the code ("verify & ship").
func resumeShip(t Task, h Hooks) Outcome {
	logf := h.Logf
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}
	setState := func(s string, r int) {
		if h.OnState != nil {
			h.OnState(s, r)
		}
	}
	repo := t.Repo
	branch := strings.NewReplacer(
		"{ticket}", strings.ToLower(t.Ticket),
		"{slug}", slugify(t.Summary),
	).Replace(repo.Branch)
	worktree := paths.WorktreeFor(t.Ticket)
	gradleHome := paths.GradleHomeFor(t.Ticket)

	if !worktreeReady(worktree) {
		setState(store.StateFailed, 0)
		return Outcome{State: store.StateFailed,
			Err: fmt.Errorf("resume: no worktree at %s — run without --resume to start fresh", worktree)}
	}
	if h.OnField != nil {
		h.OnField(branch, worktree, "")
	}
	logf("[%s] resume: verifying your changes in %s on %s", t.Ticket, worktree, branch)

	// Reuse the preserved plan/report if present (for test commands + emulator).
	plan, _ := agent.ReadPlan(worktree)
	report, _ := agent.ReadReport(worktree)

	compileCmd := repo.Compile
	if compileCmd == "" && plan != nil {
		compileCmd = plan.CompileCmd
	}
	testCmd := repo.Test
	if testCmd == "" && plan != nil {
		testCmd = plan.TestCmd
	}
	needsEmu := plan != nil && plan.NeedsEmulator && t.Cfg.AVDName != "" && t.Store != nil

	// Boot + acquire the emulator only if the original plan needed instrumented tests.
	if needsEmu {
		sdkPaths := build.ResolvePaths(t.Cfg.AndroidSDKPath)
		_ = t.Store.RegisterEmulator(t.Cfg.AVDName)
		ready := make(chan error, 1)
		go bootOrWait(t.Cfg.AVDName, sdkPaths, t.Store, logf, ready)
		logf("[%s] waiting for emulator…", t.Ticket)
		if err := <-ready; err != nil {
			logf("[%s] (warn) emulator unavailable: %v — falling back to unit tests", t.Ticket, err)
			needsEmu = false
		} else {
			for {
				if ok, _ := t.Store.AcquireEmulator(t.Cfg.AVDName, t.Ticket); ok {
					break
				}
				logf("[%s] emulator busy, waiting…", t.Ticket)
				time.Sleep(5 * time.Second)
			}
			defer func() { _ = t.Store.ReleaseEmulator(t.Cfg.AVDName) }()
		}
	}
	activeTestCmd := testCmd
	if needsEmu && repo.ConnectedTest != "" {
		activeTestCmd = repo.ConnectedTest
	}
	logf("[%s] gate (resume): compile=%q test=%q emulator=%v", t.Ticket, compileCmd, activeTestCmd, needsEmu)

	// Single gate pass — the human already fixed it; the agent does not retry.
	setState(store.StateBuilding, 0)
	if r := build.Step(worktree, gradleHome, compileCmd, "compile"); !r.OK {
		return resumeNeedsYou(t, h, setState, logf, worktree, r)
	}
	setState(store.StateTesting, 0)
	if r := build.Step(worktree, gradleHome, activeTestCmd, "test"); !r.OK {
		return resumeNeedsYou(t, h, setState, logf, worktree, r)
	}
	logf("[%s] gate green ✓", t.Ticket)

	return finishShip(t, h, worktree, branch, 1, report, logf, setState)
}

// resumeNeedsYou handles a gate failure during resume: it stops at needs-you and
// asks the human to fix the worktree again (no automatic agent retry).
func resumeNeedsYou(t Task, h Hooks, setState func(string, int), logf func(string, ...interface{}),
	worktree string, r build.Result) Outcome {
	logf("[%s] needs-you: %s still red after your changes", t.Ticket, r.Phase)
	setState(store.StateNeedsYou, 0)
	if h.Comment != nil {
		h.Comment(fmt.Sprintf(
			"🤖 *magneton [%s] — still red after resume*\n\nThe *%s* step failed on your changes.\n\n*Last error (tail):*\n{code}\n%s\n{code}\n\nFix in the worktree and resume again:\n`open -a \"Android Studio\" %s`",
			t.Ticket, r.Phase, tail(r.Output, 1500), worktree))
	}
	return Outcome{State: store.StateNeedsYou}
}

// bootOrWait either reuses an already-running emulator, starts a new one, or
// waits for another runner that is already booting it. It sends exactly one
// value on done when the emulator reaches state=ready or on error.
func bootOrWait(avdName string, p build.SDKPaths, st *store.Store, logf func(string, ...interface{}), done chan<- error) {
	// If any emulator is already attached via adb, skip booting entirely.
	if build.AlreadyRunning(p) {
		logf("[emulator] already running — reusing")
		_ = st.ReleaseEmulator(avdName) // ensure state=ready
		done <- nil
		return
	}

	state, _, err := st.EmulatorState(avdName)
	if err != nil {
		done <- fmt.Errorf("emulator state: %w", err)
		return
	}

	if state == store.EmulatorIdle {
		logf("[emulator] booting %s", avdName)
		pid, err := build.Start(p, avdName)
		if err != nil {
			done <- err
			return
		}
		won, err := st.SetEmulatorBooting(avdName, pid)
		if err != nil {
			done <- err
			return
		}
		if !won {
			// Another runner beat us to the start; kill our orphan.
			build.Kill(pid)
			logf("[emulator] start race lost — waiting for peer's boot")
		}
	}

	// Wait regardless of who launched it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := build.WaitReady(ctx, p); err != nil {
		done <- err
		return
	}
	_ = st.ReleaseEmulator(avdName) // booting → ready
	logf("[emulator] ready")
	done <- nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
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

func slugify(s string) string {
	s = strings.ToLower(s)
	s = strings.Trim(slugRe.ReplaceAllString(s, "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "change"
	}
	return s
}
