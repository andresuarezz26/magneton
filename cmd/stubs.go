package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/droidpilot/droidpilot/internal/paths"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "logs <TICKET>",
		Short: "Print the session log for a ticket",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			b, err := os.ReadFile(paths.LogFor(normalizeTicket(args[0])))
			if err != nil {
				return err
			}
			fmt.Print(string(b))
			return nil
		},
	})
}
