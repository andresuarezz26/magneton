package cmd

import (
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/jira"
	"github.com/andresuarezz26/magneton/internal/secrets"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "doctor",
		Short: "Run the connectivity check against the saved config (no prompts)",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			fmt.Println("connectivity check")
			jc := jira.New(cfg.JiraBaseURL, cfg.JiraEmail, secrets.Get(secrets.Jira))
			report("Jira", jc.Verify())
			for _, r := range cfg.Repos {
				report("git remote (origin) — "+r.Path, checkGitRemote(config.Expand(r.Path)))
			}
			report("claude CLI", exec.Command("claude", "--version").Run())
			report("gh CLI", exec.Command("gh", "auth", "status").Run())
			return nil
		},
	})
}
