package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/store"
)

func init() {
	var watch bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show the fleet: one aligned, grep-able table (Decision 8)",
		RunE: func(_ *cobra.Command, _ []string) error {
			st, err := store.Open(paths.StateDB())
			if err != nil {
				return err
			}
			defer st.Close()
			if !watch {
				return printStatus(st)
			}
			for {
				fmt.Print("\033[H\033[2J") // clear screen
				fmt.Println("agent status --watch ·", time.Now().Format("15:04:05"))
				if err := printStatus(st); err != nil {
					return err
				}
				time.Sleep(2 * time.Second)
			}
		},
	}
	c.Flags().BoolVar(&watch, "watch", false, "refresh continuously")
	rootCmd.AddCommand(c)
}

func printStatus(st *store.Store) error {
	sessions, err := st.List()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Println("no sessions yet - `agent run <TICKET>` or `agent start`")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TICKET\tSTATE\tRETRIES\tBRANCH\tAGE")
	for _, s := range sessions {
		retries := "-"
		if s.Retries > 0 {
			retries = fmt.Sprintf("%d", s.Retries)
		}
		branch := s.Branch
		if branch == "" {
			branch = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Ticket, s.State, retries, branch, age(s.UpdatedAt))
	}
	return w.Flush()
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
