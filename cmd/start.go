package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/daemon"
	"github.com/andresuarezz26/magneton/internal/paths"
)

func init() {
	var once bool
	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start the daemon: poll Jira and run the fleet (foreground; background with &)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := paths.EnsureDirs(); err != nil {
				return err
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			// Single-instance guard via pidfile.
			if pid, ok := readPid(); ok && processAlive(pid) {
				return fmt.Errorf("daemon already running (pid %d) — use `agent stop`", pid)
			}
			if err := os.WriteFile(paths.PidFile(), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
				return err
			}
			defer os.Remove(paths.PidFile())

			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return daemon.Run(ctx, cfg, once)
		},
	}
	startCmd.Flags().BoolVar(&once, "once", false, "poll a single cycle, run claimed tickets, then exit")
	rootCmd.AddCommand(startCmd)

	rootCmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		RunE: func(_ *cobra.Command, _ []string) error {
			pid, ok := readPid()
			if !ok {
				return fmt.Errorf("no daemon pidfile at %s — is it running?", paths.PidFile())
			}
			if !processAlive(pid) {
				_ = os.Remove(paths.PidFile())
				return fmt.Errorf("daemon (pid %d) not running; cleaned up stale pidfile", pid)
			}
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
				return fmt.Errorf("signal pid %d: %w", pid, err)
			}
			fmt.Printf("sent stop to daemon (pid %d)\n", pid)
			return nil
		},
	})
}

func readPid() (int, bool) {
	b, err := os.ReadFile(paths.PidFile())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		return 0, false
	}
	return pid, true
}

// processAlive reports whether a process with pid exists (signal 0 probe).
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
