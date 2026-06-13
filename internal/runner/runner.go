// Package runner is the per-ticket pipeline: worktree → agent → gate → PR.
// It is driven identically by the CLI (`agent run`) and the daemon; callers
// observe progress through Hooks (Decision 9 lifecycle states).
package runner

import (
	"fmt"
	"regexp"
	"strings"

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
}

// Hooks let the caller observe and react. Any field may be nil.
type Hooks struct {
	Logf    func(string, ...interface{})     // progress lines
	OnState func(state string, retries int)  // lifecycle transitions
	OnField func(branch, worktree, pr string) // metadata as it's known
	Comment func(text string)                // post back to the ticket
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

	report, err := agent.ReadReport(worktree)
	if err != nil {
		logf("[%s] needs-you: missing/invalid report.json (%v)", t.Ticket, err)
		setState(store.StateNeedsYou, 0)
		return Outcome{State: store.StateNeedsYou}
	}
	if report.Status == "needs_human" {
		logf("[%s] needs-you: agent reported needs_human — %s", t.Ticket, report.Summary)
		setState(store.StateNeedsYou, 0)
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
		}
	} else {
		logf("[%s] self-review: pass ✓", t.Ticket)
	}

	// 6. Build gate with bounded self-correct (Decision 4).
	attempts := 1
	gate := func() build.Result {
		setState(store.StateBuilding, attempts-1)
		if r := build.Step(worktree, gradleHome, compileCmd, "compile"); !r.OK {
			return r
		}
		setState(store.StateTesting, attempts-1)
		return build.Step(worktree, gradleHome, testCmd, "test")
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
			return Outcome{State: store.StateNeedsYou}
		}
		attempts++
		implOpts.ResumeID = sessionID
		logf("[%s] feeding %s errors back to session", t.Ticket, res.Phase)
		if sid, err := agent.Run(agent.BuildRetryPrompt(res.Phase, res.Output), implOpts); err != nil {
			logf("[%s] (warn) claude retry exited: %v", t.Ticket, err)
		} else if sid != "" {
			sessionID = sid
		}
	}

	// 7. Commit anything left uncommitted.
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

	// 8. Push + open PR (human-gated; never auto-merge).
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

	pr := vcs.PRData{
		Ticket: t.Ticket, Summary: t.Summary, Branch: branch, Base: base,
		FilesChanged: report.FilesChanged, Tests: "✅ compile + unit tests",
		Attempts: attempts, JiraBaseURL: t.Cfg.JiraBaseURL,
	}
	body, err := vcs.RenderPRBody(pr)
	if err != nil {
		return Outcome{State: store.StateFailed, Err: err}
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

	// 9. Report back on the ticket.
	if h.Comment != nil {
		pr.PRURL = prURL
		if c, err := vcs.RenderJiraComment(pr); err == nil {
			h.Comment(c)
		}
	}
	logf("[%s] review — human-gated. magneton stops here.", t.Ticket)
	return Outcome{State: store.StateReview, PRURL: prURL}
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

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
