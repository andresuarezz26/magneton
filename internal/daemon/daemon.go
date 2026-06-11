// Package daemon polls Jira and runs a concurrency-capped fleet of sessions,
// each driven by the shared runner. One machine, one daemon (Decisions 2, 5, 8).
package daemon

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/droidpilot/droidpilot/internal/config"
	"github.com/droidpilot/droidpilot/internal/jira"
	"github.com/droidpilot/droidpilot/internal/notify"
	"github.com/droidpilot/droidpilot/internal/paths"
	"github.com/droidpilot/droidpilot/internal/runner"
	"github.com/droidpilot/droidpilot/internal/secrets"
	"github.com/droidpilot/droidpilot/internal/store"
)

// Run starts the poll loop and blocks until ctx is cancelled, then drains
// in-flight sessions before returning.
func Run(ctx context.Context, cfg *config.Config) error {
	if err := paths.EnsureDirs(); err != nil {
		return err
	}
	st, err := store.Open(paths.StateDB())
	if err != nil {
		return err
	}
	defer st.Close()

	jc := jira.New(cfg.JiraBaseURL, cfg.JiraEmail, secrets.Get(secrets.Jira))
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

	logf("daemon started · concurrency %d · poll %ds", cfg.Concurrency, cfg.PollInterval)

	poll := func() {
		for i := range cfg.Repos {
			repo := &cfg.Repos[i]
			if repo.JQL == "" {
				continue
			}
			issues, err := jc.Search(repo.JQL, 50)
			if err != nil {
				logf("(warn) jira search for %s: %v", repo.Path, err)
				continue
			}
			for _, issue := range issues {
				claimed, err := st.Claim(issue.Key, repo.Path, issue.Summary)
				if err != nil {
					logf("(warn) claim %s: %v", issue.Key, err)
					continue
				}
				if !claimed {
					continue // already owned by a prior/active session
				}
				if err := jc.Transition(issue.Key, "In Progress"); err != nil {
					logf("[%s] (warn) jira transition: %v", issue.Key, err)
				}
				wg.Add(1)
				go func(issue jira.Issue, repo *config.Repo) {
					defer wg.Done()
					select {
					case sem <- struct{}{}:
					case <-ctx.Done():
						return
					}
					defer func() { <-sem }()
					process(issue, repo, cfg, st, jc)
				}(issue, repo)
			}
		}
	}

	poll() // poll once immediately on start
	ticker := time.NewTicker(time.Duration(cfg.PollInterval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logf("shutting down — draining active sessions…")
			wg.Wait()
			logf("daemon stopped")
			return nil
		case <-ticker.C:
			poll()
		}
	}
}

func process(issue jira.Issue, repo *config.Repo, cfg *config.Config, st *store.Store, jc *jira.Client) {
	ticket := issue.Key
	tlog := ticketLogger(ticket)
	tlog("[%s] %s", ticket, issue.Summary)

	out := runner.Run(runner.Task{
		Ticket: ticket, Summary: issue.Summary, Description: issue.Description,
		Repo: repo, Cfg: cfg,
	}, runner.Hooks{
		Logf:    tlog,
		OnState: func(state string, retries int) { _ = st.SetState(ticket, state, retries) },
		OnField: func(branch, worktree, pr string) { _ = st.SetFields(ticket, branch, worktree, pr) },
		Comment: func(text string) {
			if err := jc.AddComment(ticket, text); err != nil {
				tlog("[%s] (warn) jira comment failed: %v", ticket, err)
			}
		},
	})

	switch out.State {
	case store.StateReview:
		notify.Send(ticket+" · PR ready", "review and merge → "+out.PRURL)
	case store.StateNeedsYou:
		notify.Send(ticket+" · needs you", "could not land green — check the logs")
	case store.StateFailed:
		msg := "session failed"
		if out.Err != nil {
			msg = out.Err.Error()
		}
		notify.Send(ticket+" · failed", msg)
	}
	if out.Err != nil {
		tlog("[%s] error: %v", ticket, out.Err)
	}
}

// logf is the daemon-level log (stdout + daemon.log).
func logf(format string, a ...interface{}) {
	line := fmt.Sprintf(format, a...)
	fmt.Println(line)
	if f, err := os.OpenFile(paths.DaemonLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "%s  %s\n", time.Now().Format(time.RFC3339), line)
		f.Close()
	}
}

// ticketLogger writes a session's progress to stdout and ~/.agent/logs/<ticket>.log.
func ticketLogger(ticket string) func(string, ...interface{}) {
	return func(format string, a ...interface{}) {
		line := fmt.Sprintf(format, a...)
		fmt.Println(line)
		if f, err := os.OpenFile(paths.LogFor(ticket), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(f, line)
			f.Close()
		}
	}
}
