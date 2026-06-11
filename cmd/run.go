package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/droidpilot/droidpilot/internal/config"
	"github.com/droidpilot/droidpilot/internal/jira"
	"github.com/droidpilot/droidpilot/internal/paths"
	"github.com/droidpilot/droidpilot/internal/runner"
	"github.com/droidpilot/droidpilot/internal/secrets"
	"github.com/droidpilot/droidpilot/internal/store"
)

var (
	runRepo   string
	runDryRun bool
	runLocal  bool
	runTitle  string
	runDesc   string
)

func init() {
	c := &cobra.Command{
		Use:   "run <TICKET>",
		Short: "Take one Jira ticket end-to-end to an open PR",
		Args:  cobra.ExactArgs(1),
		RunE:  runE,
	}
	c.Flags().StringVar(&runRepo, "repo", "", "repo path (defaults to the first configured repo)")
	c.Flags().BoolVar(&runDryRun, "dry-run", false, "do everything except git push and PR (Decision 16)")
	c.Flags().BoolVar(&runLocal, "local", false, "skip Jira: take ticket text from --title/--desc (for testing)")
	c.Flags().StringVar(&runTitle, "title", "", "ticket summary (requires --local)")
	c.Flags().StringVar(&runDesc, "desc", "", "ticket description (with --local)")
	rootCmd.AddCommand(c)
}

func runE(_ *cobra.Command, args []string) error {
	ticket := normalizeTicket(args[0])
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

	logf, closeLog := newLogger(ticket)
	defer closeLog()

	// Resolve the ticket — from Jira, or from flags in --local test mode.
	var jc *jira.Client
	summary, desc := runTitle, runDesc
	if runLocal {
		if runTitle == "" {
			return fmt.Errorf("--local requires --title")
		}
		logf("[%s] queued → working (local)", ticket)
	} else {
		jc = jira.New(cfg.JiraBaseURL, cfg.JiraEmail, secrets.Get(secrets.Jira))
		logf("[%s] queued → working", ticket)
		issue, err := jc.FetchIssue(ticket)
		if err != nil {
			return fmt.Errorf("fetch ticket: %w", err)
		}
		summary, desc = issue.Summary, issue.Description
	}
	logf("[%s] %s", ticket, summary)

	// State store so `agent status` reflects manual runs too.
	st, err := store.Open(paths.StateDB())
	if err != nil {
		return err
	}
	defer st.Close()
	_, _ = st.Claim(ticket, repo.Path, summary)

	hooks := runner.Hooks{
		Logf:    logf,
		OnState: func(state string, retries int) { _ = st.SetState(ticket, state, retries) },
		OnField: func(branch, worktree, pr string) { _ = st.SetFields(ticket, branch, worktree, pr) },
	}
	if jc != nil {
		hooks.Comment = func(text string) {
			if err := jc.AddComment(ticket, text); err != nil {
				logf("[%s] (warn) jira comment failed: %v", ticket, err)
			}
		}
	}

	out := runner.Run(runner.Task{
		Ticket: ticket, Summary: summary, Description: desc,
		Repo: repo, Cfg: cfg, DryRun: runDryRun,
	}, hooks)
	return out.Err
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
