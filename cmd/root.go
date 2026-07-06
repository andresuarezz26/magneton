// Package cmd wires up the `agent` CLI (Decision 7: headless daemon + CLI).
package cmd

import (
	"strings"

	"github.com/spf13/cobra"
)

// version is overridden at release time via -ldflags -X.
var version = "dev"

var rootCmd = &cobra.Command{
	Use:           "magneton",
	Short:         "magneton - autonomous Android ticket → PR agent",
	Version:       version,
	SilenceUsage:  true,
	SilenceErrors: true,
	// Bare `agent` (no subcommand) opens the TUI hub. Subcommands still work.
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error { return launchHub() },
}

// Execute runs the root command.
func Execute() error { return rootCmd.Execute() }

func normalizeTicket(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }
