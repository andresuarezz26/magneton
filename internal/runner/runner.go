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

// Run executes the full pipeline for one ticket.
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

	// 1. Provision an isolated worktree (Decision 7).
	setState(store.StateWorking, 0)
	logf("[%s] worktree %s on %s", t.Ticket, worktree, branch)
	if err := git.CreateWorktree(repo.Path, worktree, branch, repo.Base); err != nil {
		setState(store.StateFailed, 0)
		return Outcome{State: store.StateFailed, Err: fmt.Errorf("create worktree: %w", err)}
	}
	if h.OnField != nil {
		h.OnField(branch, worktree, "")
	}

	// 2. Drive the agent.
	opts := agent.Options{
		WorktreeDir:  worktree,
		AllowedTools: t.Cfg.AllowedTools,
		MaxBudgetUSD: t.Cfg.MaxBudgetUSD,
		AnthropicKey: secrets.Get(secrets.Anthropic),
		Logf:         logf,
	}
	logf("[%s] working → driving claude code", t.Ticket)
	sessionID, runErr := agent.Run(
		agent.BuildPrompt(t.Ticket, t.Summary, t.Description, repo.Compile, repo.Test), opts)
	if runErr != nil {
		logf("[%s] (warn) claude exited: %v", t.Ticket, runErr)
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
	logf("[%s] agent done: %s", t.Ticket, report.Summary)

	// 3. Build gate with bounded self-correct (Decision 4).
	attempts := 1
	gate := func() build.Result {
		setState(store.StateBuilding, attempts-1)
		if r := build.Step(worktree, gradleHome, repo.Compile, "compile"); !r.OK {
			return r
		}
		setState(store.StateTesting, attempts-1)
		return build.Step(worktree, gradleHome, repo.Test, "test")
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
		opts.ResumeID = sessionID
		logf("[%s] feeding %s errors back to session", t.Ticket, res.Phase)
		if sid, err := agent.Run(agent.BuildRetryPrompt(res.Phase, res.Output), opts); err != nil {
			logf("[%s] (warn) claude retry exited: %v", t.Ticket, err)
		} else if sid != "" {
			sessionID = sid
		}
	}

	// 4. Commit anything left uncommitted.
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

	// 5. Push + open PR (human-gated; never auto-merge).
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

	// 6. Report back on the ticket.
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
