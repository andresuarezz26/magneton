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
	Ship        bool         // ship without verifying: trust the human's fix, commit + push + PR directly
	Store       *store.Store // optional; enables emulator coordination
	Images      []string     // image files to make the agent see (pasted-content flow)
	Base        string       // bare base branch name for stacked diffs; "" = default
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
	if t.Ship {
		return shipOnly(t, h)
	}
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
	branch := resolveBranch(repo.Branch, t.Ticket, t.Summary)
	worktree := paths.WorktreeFor(repo.Path, t.Ticket)
	anthropicKey := secrets.Get(secrets.Anthropic)

	// 1. Provision an isolated worktree (Decision 7).
	setState(store.StatePlanning, 0)
	// Resolve the stacked base: the flag wins, but on a plain re-run (no --base)
	// fall back to the value persisted at creation time so re-running a stacked
	// ticket doesn't silently reset it to origin/<default> and drop the parent's
	// commits. (finishShip reads the same stored value for the PR base.)
	stackBase := t.Base
	if stackBase == "" && t.Store != nil {
		if sess, err := t.Store.Get(t.Ticket); err == nil && sess != nil {
			stackBase = sess.BaseBranch
		}
	}
	// Resolve the base ref: prefer a local branch (covers in-progress parents),
	// then fall back to origin/<name>, then origin/<default>. Validate before
	// creating the worktree so a bad base surfaces a clear error rather than a
	// raw git message.
	baseRef := "origin/" + func() string {
		if repo.Base != "" {
			return repo.Base
		}
		return git.DefaultBranch(repo.Path)
	}()
	if stackBase != "" {
		if git.RefExists(repo.Path, stackBase) {
			baseRef = stackBase // local branch (e.g. in-progress parent)
		} else if git.RefExists(repo.Path, "origin/"+stackBase) {
			baseRef = "origin/" + stackBase
		} else {
			setState(store.StateNeedsYou, 0)
			return Outcome{State: store.StateNeedsYou,
				Err: fmt.Errorf("stack base %q: no local or remote branch found", stackBase)}
		}
	}
	logf("[%s] worktree %s on %s (base: %s)", t.Ticket, worktree, branch, baseRef)
	if err := git.CreateWorktree(repo.Path, worktree, branch, baseRef); err != nil {
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

	// Copy any attached images into the worktree so the agent's Read tool can view
	// them, and point the description at them. .agent/ is git-excluded, so the
	// images never land in the PR.
	desc := stageImages(t, worktree, logf)

	// 2. PLAN stage - strong model, read-only tools.
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
		SettingsJSON: t.Cfg.SandboxSettingsJSON(),
		Logf:         logf,
	}
	if _, err := agent.Run(agent.BuildPlanPrompt(t.Ticket, t.Summary, desc), planOpts); err != nil {
		logf("[%s] (warn) plan stage exited: %v", t.Ticket, err)
	}

	plan, err := agent.ReadPlan(worktree)
	if err != nil {
		logf("[%s] needs-you: plan stage did not produce plan.json (%v)", t.Ticket, err)
		setState(store.StateNeedsYou, 0)
		needsYouComment(fmt.Sprintf("The plan stage failed to produce a plan - the ticket may be too ambiguous or the codebase too complex to analyse automatically.\n\nError: `%v`", err))
		return Outcome{State: store.StateNeedsYou}
	}
	logf("[%s] plan (%s/%s): %s", t.Ticket, plan.Type, plan.Confidence, oneLine(plan.Plan, 120))

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

	// 3. CLARIFY - always post plan to Jira; stop if there are blocking questions.
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

	// 4. IMPLEMENT stage - fast model, full tools, plan injected.
	setState(store.StateWorking, 0)
	logf("[%s] stage:implement model:%s", t.Ticket, modelImpl)
	implOpts := agent.Options{
		WorktreeDir:  worktree,
		AllowedTools: t.Cfg.AllowedTools,
		Model:        modelImpl,
		MaxBudgetUSD: t.Cfg.MaxBudgetUSD,
		AnthropicKey: anthropicKey,
		SettingsJSON: t.Cfg.SandboxSettingsJSON(),
		Logf:         logf,
	}
	sessionID, runErr := agent.Run(
		agent.BuildImplPrompt(t.Ticket, t.Summary, desc, plan),
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
		needsYouComment("The implement stage ended without producing a completion report - the session likely crashed or timed out mid-run.")
		return Outcome{State: store.StateNeedsYou}
	}
	if report.Status == "needs_human" {
		logf("[%s] needs-you: agent reported needs_human - %s", t.Ticket, report.Summary)
		setState(store.StateNeedsYou, 0)
		needsYouComment(fmt.Sprintf("The agent determined it cannot safely complete this ticket automatically:\n\n> %s", report.Summary))
		return Outcome{State: store.StateNeedsYou}
	}
	logf("[%s] implement done: %s", t.Ticket, report.Summary)

	// 5. SELF-REVIEW - adversarial diff review; one fix round if issues found.
	setState(store.StateReviewing, 0)
	logf("[%s] stage:review model:%s", t.Ticket, modelReview)
	reviewOpts := agent.Options{
		WorktreeDir:  worktree,
		AllowedTools: agent.ReviewTools,
		Model:        modelReview,
		MaxBudgetUSD: t.Cfg.MaxBudgetUSD * 0.3,
		AnthropicKey: anthropicKey,
		SettingsJSON: t.Cfg.SandboxSettingsJSON(),
		Logf:         logf,
	}
	if _, err := agent.Run(agent.BuildReviewPrompt(t.Ticket, t.Summary, plan), reviewOpts); err != nil {
		logf("[%s] (warn) review stage exited: %v", t.Ticket, err)
	}
	if review, err := agent.ReadReview(worktree); err != nil {
		logf("[%s] (warn) no review.json - skipping self-review gate", t.Ticket)
	} else if review.Verdict == "fix" && len(review.Issues) > 0 {
		logf("[%s] self-review: %d issue(s) - applying one fix round", t.Ticket, len(review.Issues))
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

	// 6. Verify. Sync with the emulator and acquire it first (the agent runs any
	// instrumented tests against it).
	if needsEmu {
		logf("[%s] waiting for emulator…", t.Ticket)
		if err := <-emulatorReady; err != nil {
			logf("[%s] (warn) emulator unavailable: %v - falling back to unit tests", t.Ticket, err)
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

	// VERIFY - the agent discovers and runs this project's own build + tests
	// itself (per-project Gradle setups and company build skills all work), fixes
	// failures, and certifies the result in report.json. magneton trusts that
	// verdict instead of running hardcoded Gradle commands. Because the agent's
	// process inherits the real environment, this also sidesteps the isolated-Gradle
	// TLS/cert failures the orchestrator-run gate hit on locked-down machines.
	logf("[%s] stage:verify - agent will discover & run build + tests (emulator=%v)", t.Ticket, needsEmu)
	vreport, verified, sid := verifyWithAgent(t, worktree, sessionID, needsEmu, true, anthropicKey, logf, setState)
	if sid != "" {
		sessionID = sid
		saveSession(sessionID)
	}
	if vreport != nil {
		report = vreport
	}
	if !verified {
		reason := "the agent could not get the build and tests green."
		if vreport != nil && strings.TrimSpace(vreport.VerifyLog) != "" {
			reason = vreport.VerifyLog
		}
		logf("[%s] needs-you: agent did not certify the build (verified=false)", t.Ticket)
		setState(store.StateNeedsYou, 0)
		needsYouComment(fmt.Sprintf(
			"The agent ran this project's build and tests but could not certify them green:\n\n{code}\n%s\n{code}\n\nOpen the worktree in Android Studio to investigate:\n`open -a \"Android Studio\" %s`",
			tail(reason, 1500), worktree))
		return Outcome{State: store.StateNeedsYou}
	}
	logf("[%s] verify: agent certified build + tests green ✓", t.Ticket)

	// 7-9. Commit, push, open PR (shared with the resume path).
	return finishShip(t, h, worktree, branch, 1, report, logf, setState)
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
		logf("[%s] dry-run: branch %s ready - skipping push + PR", t.Ticket, branch)
		setState(store.StateReview, attempts-1)
		return Outcome{State: store.StateReview}
	}

	// PR base: use the stacked base if set, else load from the store (covers
	// resume/ship where t.Base is "" but the base was recorded at worktree time),
	// else fall back to the repo default.
	base := t.Base
	if base == "" && t.Store != nil {
		if sess, err := t.Store.Get(t.Ticket); err == nil && sess != nil {
			base = sess.BaseBranch
		}
	}
	if base == "" {
		base = repo.Base
	}
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
		// Completeness repair: restore any sections the LLM dropped.
		if tmpl := vcs.ReadRepoTemplate(worktree); tmpl != "" {
			if missing := vcs.MissingSections(tmpl, body); len(missing) > 0 {
				logf("[%s] PR body was missing %d template section(s) - restored from template", t.Ticket, len(missing))
				body = vcs.RepairSections(tmpl, body, missing)
			}
		}
	} else {
		b, err := vcs.RenderPRBody(pr)
		if err != nil {
			return Outcome{State: store.StateFailed, Err: err}
		}
		body = b
	}
	prTitle := prTitleFor(worktree, t.Ticket, t.Summary)
	prURL, err := vcs.OpenPR(worktree, base, prTitle, body)
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
	logf("[%s] review - human-gated. magneton stops here.", t.Ticket)
	return Outcome{State: store.StateReview, PRURL: prURL}
}

// archiveReport persists a ticket's completion report into magneton's own home
// (~/.agent/reports/<ticket>.json) so it survives outside the worktree and can
// later be surfaced by a report viewer - without ever being committed to the
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

// verifyWithAgent has the Claude session itself discover and run this project's
// build + tests, then reads back the verdict the agent wrote into report.json.
// When allowFix is true the agent may edit code to make it pass; when false
// (resume) it only confirms a human's existing fix and must not edit. emulator
// tells the agent whether instrumented tests can run. Returns the refreshed
// report (nil if the agent left none), whether the agent certified success, and
// the (possibly new) Claude session id.
func verifyWithAgent(t Task, worktree, sessionID string,
	emulator, allowFix bool, anthropicKey string,
	logf func(string, ...interface{}), setState func(string, int)) (*agent.Report, bool, string) {

	setState(store.StateBuilding, 0)
	vOpts := agent.Options{
		WorktreeDir:  worktree,
		AllowedTools: t.Cfg.AllowedTools,
		Model:        t.Cfg.ModelImpl,
		MaxBudgetUSD: t.Cfg.MaxBudgetUSD,
		AnthropicKey: anthropicKey,
		ResumeID:     sessionID,
		SettingsJSON: t.Cfg.SandboxSettingsJSON(),
		Logf:         logf,
	}
	newSession := sessionID
	if sid, err := agent.Run(
		agent.BuildVerifyPrompt(t.Ticket, t.Summary, emulator, allowFix),
		vOpts); err != nil {
		logf("[%s] (warn) verify stage exited: %v", t.Ticket, err)
	} else if sid != "" {
		newSession = sid
	}
	rep, err := agent.ReadReport(worktree)
	if err != nil {
		logf("[%s] (warn) verify produced no readable report.json (%v)", t.Ticket, err)
		return nil, false, newSession
	}
	return rep, rep.Verified != nil && *rep.Verified, newSession
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
	branch := resolveBranch(repo.Branch, t.Ticket, t.Summary)
	worktree := paths.WorktreeFor(repo.Path, t.Ticket)

	if !worktreeReady(worktree) {
		setState(store.StateFailed, 0)
		return Outcome{State: store.StateFailed,
			Err: fmt.Errorf("resume: no worktree at %s - run without --resume to start fresh", worktree)}
	}
	if h.OnField != nil {
		h.OnField(branch, worktree, "")
	}
	logf("[%s] resume: verifying your changes in %s on %s", t.Ticket, worktree, branch)

	// Reuse the preserved plan/report if present (for the emulator decision).
	plan, _ := agent.ReadPlan(worktree)
	report, _ := agent.ReadReport(worktree)

	needsEmu := plan != nil && plan.NeedsEmulator && t.Cfg.AVDName != "" && t.Store != nil

	// Boot + acquire the emulator only if the original plan needed instrumented tests.
	if needsEmu {
		sdkPaths := build.ResolvePaths(t.Cfg.AndroidSDKPath)
		_ = t.Store.RegisterEmulator(t.Cfg.AVDName)
		ready := make(chan error, 1)
		go bootOrWait(t.Cfg.AVDName, sdkPaths, t.Store, logf, ready)
		logf("[%s] waiting for emulator…", t.Ticket)
		if err := <-ready; err != nil {
			logf("[%s] (warn) emulator unavailable: %v - falling back to unit tests", t.Ticket, err)
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
	anthropicKey := secrets.Get(secrets.Anthropic)
	logf("[%s] verify (resume): agent will run build + tests on your changes (emulator=%v)", t.Ticket, needsEmu)

	// The human already fixed the code; the agent only RUNS the build + tests to
	// confirm it (allowFix=false - it never edits) and self-certifies in report.json.
	vreport, verified, _ := verifyWithAgent(t, worktree, "", needsEmu, false, anthropicKey, logf, setState)
	if !verified {
		reason := "the build/tests are still red on your changes."
		if vreport != nil && strings.TrimSpace(vreport.VerifyLog) != "" {
			reason = vreport.VerifyLog
		}
		return resumeNeedsYou(t, h, setState, logf, worktree, reason)
	}
	if vreport != nil {
		report = vreport
	}
	logf("[%s] verify (resume): agent certified your fix builds + passes ✓", t.Ticket)

	return finishShip(t, h, worktree, branch, 1, report, logf, setState)
}

// shipOnly trusts the human's fix completely: it SKIPS verification entirely and
// goes straight to commit + push + PR from the existing worktree. It's the escape
// hatch for when verification itself is the unreliable part - e.g. sandbox or
// environment constraints in the worktree that fail the build regardless of the
// code (the Kotlin Native cache .lock case), where re-running the gate would loop
// in needs-you forever no matter how good the fix is. The human has confirmed it
// by hand; magneton takes their word and ships.
func shipOnly(t Task, h Hooks) Outcome {
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
	branch := resolveBranch(repo.Branch, t.Ticket, t.Summary)
	worktree := paths.WorktreeFor(repo.Path, t.Ticket)

	if !worktreeReady(worktree) {
		setState(store.StateFailed, 0)
		return Outcome{State: store.StateFailed,
			Err: fmt.Errorf("ship: no worktree at %s - run without --ship to start fresh", worktree)}
	}
	if h.OnField != nil {
		h.OnField(branch, worktree, "")
	}
	logf("[%s] ship: trusting your fix - skipping verification, committing + opening PR", t.Ticket)

	// Reuse the last report (for PR body/files) if one survived; finishShip
	// tolerates a nil report.
	report, _ := agent.ReadReport(worktree)
	return finishShip(t, h, worktree, branch, 1, report, logf, setState)
}

// resumeNeedsYou handles a failed verification during resume: it stops at
// needs-you and asks the human to fix the worktree again (the agent never edits
// on the resume path). reason is the agent's verifyLog (or a fallback).
func resumeNeedsYou(t Task, h Hooks, setState func(string, int), logf func(string, ...interface{}),
	worktree, reason string) Outcome {
	logf("[%s] needs-you: build/tests still red after your changes", t.Ticket)
	setState(store.StateNeedsYou, 0)
	if h.Comment != nil {
		h.Comment(fmt.Sprintf(
			"🤖 *magneton [%s] - still red after resume*\n\nThe build or tests failed on your changes.\n\n*What the agent saw (tail):*\n{code}\n%s\n{code}\n\nFix in the worktree and resume again:\n`open -a \"Android Studio\" %s`",
			t.Ticket, tail(reason, 1500), worktree))
	}
	return Outcome{State: store.StateNeedsYou}
}

// bootOrWait either reuses an already-running emulator, starts a new one, or
// waits for another runner that is already booting it. It sends exactly one
// value on done when the emulator reaches state=ready or on error.
func bootOrWait(avdName string, p build.SDKPaths, st *store.Store, logf func(string, ...interface{}), done chan<- error) {
	// If any emulator is already attached via adb, skip booting entirely.
	if build.AlreadyRunning(p) {
		logf("[emulator] already running - reusing")
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
			logf("[emulator] start race lost - waiting for peer's boot")
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

// stageImages copies the task's attached images into <worktree>/.agent/images and
// returns the description with a reference section appended so the plan/implement
// prompts tell the agent to Read them. Returns the plain description on any issue.
func stageImages(t Task, worktree string, logf func(string, ...interface{})) string {
	if len(t.Images) == 0 {
		return t.Description
	}
	dir := filepath.Join(worktree, ".agent", "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logf("[%s] (warn) image dir: %v", t.Ticket, err)
		return t.Description
	}
	var refs []string
	for i, src := range t.Images {
		name := fmt.Sprintf("img-%d%s", i+1, strings.ToLower(filepath.Ext(src)))
		data, err := os.ReadFile(src)
		if err != nil {
			logf("[%s] (warn) read image %s: %v", t.Ticket, src, err)
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			logf("[%s] (warn) write image %s: %v", t.Ticket, name, err)
			continue
		}
		refs = append(refs, ".agent/images/"+name)
	}
	if len(refs) == 0 {
		return t.Description
	}
	return t.Description +
		"\n\nAttached screenshots - use the Read tool to view each before planning and implementing:\n- " +
		strings.Join(refs, "\n- ")
}

// resolveBranch fills a repo's branch pattern for a ticket, substituting the
// {ticket} (lowercased id) and {slug} (kebab-case title) placeholders.
func resolveBranch(pattern, ticket, summary string) string {
	return strings.NewReplacer(
		"{ticket}", strings.ToLower(ticket),
		"{slug}", slugify(summary),
	).Replace(pattern)
}

// prTitleFor builds the PR title, prepending a [feat]/[bug]/[chore] prefix
// when the ticket's plan.json records a type.
func prTitleFor(worktree, ticket, summary string) string {
	prefix := ""
	if p, err := agent.ReadPlan(worktree); err == nil && p != nil {
		switch p.Type {
		case "feature":
			prefix = "[feat]"
		case "bug", "chore":
			prefix = "[" + p.Type + "]"
		}
	}
	return fmt.Sprintf("%s[%s] %s", prefix, ticket, summary)
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
