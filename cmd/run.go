package cmd

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/jira"
	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/runner"
	"github.com/andresuarezz26/magneton/internal/secrets"
	"github.com/andresuarezz26/magneton/internal/store"
)

var (
	runRepo   string
	runDryRun bool
	runLocal  bool
	runTitle  string
	runDesc   string
	runResume bool
)

func init() {
	c := &cobra.Command{
		Use:   "run <TICKET|FILE>...",
		Short: "Take one or more tickets (Jira keys or local .md files) end-to-end to open PRs",
		Args:  cobra.MinimumNArgs(1),
		RunE:  runE,
	}
	c.Flags().StringVar(&runRepo, "repo", "", "repo path (defaults to the first configured repo)")
	c.Flags().BoolVar(&runDryRun, "dry-run", false, "do everything except git push and PR (Decision 16)")
	c.Flags().BoolVar(&runLocal, "local", false, "skip Jira: take ticket text from --title/--desc (for testing)")
	c.Flags().StringVar(&runTitle, "title", "", "ticket summary (requires --local)")
	c.Flags().StringVar(&runDesc, "desc", "", "ticket description (with --local)")
	c.Flags().BoolVar(&runResume, "resume", false, "verify & ship: continue from the existing worktree (keep manual fixes), re-run the gate, then PR")
	rootCmd.AddCommand(c)
}

func runE(_ *cobra.Command, args []string) error {
	if err := paths.EnsureDirs(); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	repo, err := cfg.Resolve(runRepo)
	if err != nil {
		return err
	}

	specs, err := resolveSpecs(args)
	if err != nil {
		return err
	}

	// One shared store across goroutines (its *sql.DB is concurrency-safe;
	// the daemon shares one the same way).
	st, err := store.Open(paths.StateDB())
	if err != nil {
		return err
	}
	defer st.Close()

	// Fan out, capped at cfg.Concurrency, mirroring the daemon's worker pool.
	// No ctx: the CLI is foreground and runs every ticket to completion.
	conc := cfg.Concurrency
	if conc < 1 {
		conc = 1
	}
	var (
		wg     sync.WaitGroup
		sem    = make(chan struct{}, conc)
		mu     sync.Mutex
		failed int
	)
	for _, sp := range specs {
		wg.Add(1)
		go func(sp ticketSpec) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out := runOne(sp, cfg, repo, st)
			if out.Err != nil || out.State == store.StateFailed {
				mu.Lock()
				failed++
				mu.Unlock()
			}
		}(sp)
	}
	wg.Wait()

	if failed > 0 {
		return fmt.Errorf("%d of %d ticket(s) failed", failed, len(specs))
	}
	return nil
}

// resolveSpecs turns CLI args into resolved, de-duplicated ticketSpecs, failing
// fast on validation before any work fans out. Per arg, in order:
//  1. legacy --local/--title (single arg only): take text from flags;
//  2. an existing file on disk: parse it as a local .md/text ticket;
//  3. otherwise: a Jira ticket key.
func resolveSpecs(args []string) ([]ticketSpec, error) {
	multi := len(args) > 1
	if multi && (runTitle != "" || runDesc != "") {
		return nil, fmt.Errorf("--title/--desc cannot be combined with multiple tickets; put the text in the .md files")
	}

	specs := make([]ticketSpec, 0, len(args))
	for _, arg := range args {
		// (1) Legacy flag-driven local mode, single arg only.
		if !multi && (runLocal || runTitle != "") {
			if runTitle == "" {
				return nil, fmt.Errorf("--local requires --title")
			}
			specs = append(specs, ticketSpec{
				ticket: normalizeTicket(arg), summary: runTitle, desc: runDesc, local: true,
			})
			continue
		}
		// (2) An existing file → local source.
		if fi, err := os.Stat(arg); err == nil && !fi.IsDir() {
			sp, err := loadLocalTicket(arg)
			if err != nil {
				return nil, err
			}
			specs = append(specs, sp)
			continue
		}
		// (3) Jira ticket key.
		specs = append(specs, ticketSpec{ticket: normalizeTicket(arg)})
	}

	return dedupeSpecs(specs), nil
}

// dedupeSpecs guarantees unique ticket ids within one invocation so two files
// with the same basename (both derive e.g. "DUP") don't collide on
// worktree/branch/log path or the store's primary key. Later collisions get a
// -2/-3 suffix.
func dedupeSpecs(specs []ticketSpec) []ticketSpec {
	seen := map[string]int{}
	for i := range specs {
		id := specs[i].ticket
		if n := seen[id]; n > 0 {
			newID := fmt.Sprintf("%s-%d", id, n+1)
			for seen[newID] > 0 {
				n++
				newID = fmt.Sprintf("%s-%d", id, n+1)
			}
			fmt.Printf("(warn) ticket id %q collides; using %q\n", id, newID)
			seen[id] = n + 1
			specs[i].ticket = newID
			seen[newID] = 1
		} else {
			seen[id] = 1
		}
	}
	return specs
}

// runOne executes the full pipeline for a single resolved ticket. It never
// bubbles an error up the Go path: it returns an Outcome so sibling tickets in
// a parallel run keep going.
func runOne(sp ticketSpec, cfg *config.Config, repo *config.Repo, st *store.Store) runner.Outcome {
	logf, closeLog := newLogger(sp.ticket)
	defer closeLog()

	summary, desc := sp.summary, sp.desc
	var jc *jira.Client
	if sp.local {
		logf("[%s] queued → working (local)", sp.ticket)
	} else {
		jc = jira.New(cfg.JiraBaseURL, cfg.JiraEmail, secrets.Get(secrets.Jira))
		logf("[%s] queued → working", sp.ticket)
		issue, err := jc.FetchIssue(sp.ticket)
		if err != nil {
			logf("[%s] (error) fetch ticket: %v", sp.ticket, err)
			return runner.Outcome{State: store.StateFailed, Err: fmt.Errorf("fetch %s: %w", sp.ticket, err)}
		}
		summary, desc = issue.Summary, issue.Description
		if !strings.EqualFold(issue.Status, cfg.JiraInProgressStatus) {
			logf("[%s] status is %q — transitioning to %q", sp.ticket, issue.Status, cfg.JiraInProgressStatus)
			if err := jc.TransitionTo(sp.ticket, cfg.JiraInProgressStatus); err != nil {
				logf("[%s] (warn) could not transition to %q: %v", sp.ticket, cfg.JiraInProgressStatus, err)
			}
		}
	}

	// Append any locally-saved answers (from the TUI answer box) to the
	// description so the agent sees them without a Jira round-trip.
	if raw, err := os.ReadFile(paths.AnswerFor(sp.ticket)); err == nil && len(raw) > 0 {
		desc = strings.TrimSpace(desc) + "\n\n---\nAnswers:\n" + strings.TrimSpace(string(raw))
		_ = os.Remove(paths.AnswerFor(sp.ticket)) // consume once
		logf("[%s] local answers injected into description", sp.ticket)
	}
	logf("[%s] %s", sp.ticket, summary)

	// State store so `magneton status` reflects manual runs too.
	_, _ = st.Claim(sp.ticket, repo.Path, summary)
	_ = st.SetPID(sp.ticket, os.Getpid()) // for monitor liveness (kill -0)
	if sp.sourcePath != "" {
		_ = st.SetSourcePath(sp.ticket, sp.sourcePath)
	}
	// Reset state immediately so a re-run leaves a stale terminal state
	// (failed/needs-you/stopped/review) right away instead of lingering there
	// until the pipeline reaches planning (after the slow worktree setup).
	_ = st.SetState(sp.ticket, store.StateQueued, 0)

	hooks := runner.Hooks{
		Logf:    logf,
		OnState: func(state string, retries int) { _ = st.SetState(sp.ticket, state, retries) },
		OnField: func(branch, worktree, pr string) { _ = st.SetFields(sp.ticket, branch, worktree, pr) },
	}
	if jc != nil {
		hooks.Comment = func(text string) {
			if err := jc.AddComment(sp.ticket, text); err != nil {
				logf("[%s] (warn) jira comment failed: %v", sp.ticket, err)
			}
		}
	} else {
		// No Jira: route the plan + any blocking questions to the CLI so they
		// don't vanish (the runner only emits them via Comment).
		hooks.Comment = localPlanComment(logf, sp.ticket)
	}

	out := runner.Run(runner.Task{
		Ticket: sp.ticket, Summary: summary, Description: desc,
		Repo: repo, Cfg: cfg, DryRun: runDryRun, Resume: runResume,
		Store: st,
	}, hooks)
	// Record the terminal outcome in the ticket log so the reason is visible in
	// the TUI/`agent logs` even when stdout/stderr is discarded (TUI-launched).
	switch {
	case out.Err != nil:
		logf("[%s] ✗ %s: %v", sp.ticket, out.State, out.Err)
	case out.State == store.StateReview:
		logf("[%s] ✓ review — PR ready: %s", sp.ticket, out.PRURL)
	default:
		logf("[%s] ended in state: %s", sp.ticket, out.State)
	}
	return out
}

// localPlanComment routes runner Comment text (the rendered plan and blocking
// questions) to the CLI logger so it's visible without Jira. Emitted as a
// single logf call so the block doesn't interleave with other tickets' lines.
func localPlanComment(logf func(string, ...interface{}), ticket string) func(string) {
	return func(text string) {
		logf("[%s] ----- agent comment -----\n%s\n[%s] -------------------------", ticket, text, ticket)
	}
}

// newLogger returns a logf that writes to stdout and ~/.agent/logs/<ticket>.log.
func newLogger(ticket string) (func(string, ...interface{}), func()) {
	f, _ := os.OpenFile(paths.LogFor(ticket), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	logf := func(format string, a ...interface{}) {
		line := fmt.Sprintf(format, a...)
		fmt.Println(line)
		if f != nil {
			fmt.Fprintln(f, line)
		}
	}
	return logf, func() {
		if f != nil {
			f.Close()
		}
	}
}
