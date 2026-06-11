// Package cmd wires up the `agent` CLI (Decision 7: headless daemon + CLI).
package cmd

import (
	"strings"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:           "agent",
	Short:         "droidpilot — autonomous Android ticket → PR agent",
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the root command.
func Execute() error { return rootCmd.Execute() }

func normalizeTicket(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }
