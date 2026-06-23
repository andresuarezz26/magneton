// Package daemon runs a periodic cleanup loop for the background agent fleet.
// Ticket intake is done via `magneton run` (Phase 1); the daemon owns cleanup
// and emulator lifecycle only (Decisions 2, 5, 8 — JQL polling deferred).
package daemon

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/andresuarezz26/magneton/internal/build"
	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/git"
	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/store"
	"github.com/andresuarezz26/magneton/internal/vcs"
)

// Run starts the poll loop and blocks until ctx is cancelled.
// When once is true it runs a single cycle and returns (handy for live testing).
func Run(ctx context.Context, cfg *config.Config, once bool) error {
	if err := paths.EnsureDirs(); err != nil {
		return err
	}
	st, err := store.Open(paths.StateDB())
	if err != nil {
		return err
	}
	defer st.Close()

	logf("daemon started · poll %ds", cfg.PollInterval)

	poll := func() {
		cleanupResolved(st)
		idleShutdownEmulators(st, cfg)
	}

	poll()

	if once {
		logf("once: cycle complete")
		return nil
	}

	ticker := time.NewTicker(time.Duration(cfg.PollInterval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logf("shutting down")
			shutdownEmulators(st, cfg)
			logf("daemon stopped")
			return nil
		case <-ticker.C:
			poll()
		}
	}
}

// cleanupResolved removes the worktree and marks the session merged/closed once
// its PR is no longer open (Decision 7).
func cleanupResolved(st *store.Store) {
	sessions, err := st.List()
	if err != nil {
		return
	}
	for _, s := range sessions {
		if s.State != store.StateReview || s.PRURL == "" || s.Repo == "" {
			continue
		}
		state, err := vcs.PRState(s.Repo, s.PRURL)
		if err != nil {
			continue
		}
		switch state {
		case "MERGED", "CLOSED":
			if s.Worktree != "" {
				if err := git.RemoveWorktree(s.Repo, s.Worktree); err != nil {
					logf("[%s] (warn) worktree cleanup: %v", s.Ticket, err)
				}
			}
			next := store.StateClosed
			if state == "MERGED" {
				next = store.StateMerged
			}
			_ = st.SetState(s.Ticket, next, s.Retries)
			logf("[%s] %s — worktree reclaimed", s.Ticket, next)
		}
	}
}

// idleShutdownEmulators kills any emulator idle past cfg.EmulatorIdleTimeout minutes.
func idleShutdownEmulators(st *store.Store, cfg *config.Config) {
	if cfg.AVDName == "" {
		return
	}
	state, pid, err := st.EmulatorState(cfg.AVDName)
	if err != nil || state != store.EmulatorReady {
		return
	}
	lastUsed, err := st.EmulatorLastUsed(cfg.AVDName)
	if err != nil {
		return
	}
	idleSecs := int64(cfg.EmulatorIdleTimeout) * 60
	if time.Now().Unix()-lastUsed < idleSecs {
		return
	}
	logf("[emulator] idle timeout — shutting down %s (pid %d)", cfg.AVDName, pid)
	build.Kill(pid)
	_ = st.SetEmulatorIdle(cfg.AVDName)
}

// shutdownEmulators kills all running emulators on daemon shutdown.
func shutdownEmulators(st *store.Store, cfg *config.Config) {
	if cfg.AVDName == "" {
		return
	}
	state, pid, err := st.EmulatorState(cfg.AVDName)
	if err != nil || state == store.EmulatorIdle || pid == 0 {
		return
	}
	logf("[emulator] daemon shutdown — killing %s (pid %d)", cfg.AVDName, pid)
	build.Kill(pid)
	_ = st.SetEmulatorIdle(cfg.AVDName)
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
