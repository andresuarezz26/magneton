package cmd

import (
	"errors"
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/jira"
	"github.com/andresuarezz26/magneton/internal/secrets"
)

func checkGH() error {
	if _, err := exec.LookPath("gh"); err != nil {
		return errors.New("not found in PATH")
	}
	if err := exec.Command("gh", "auth", "status").Run(); err != nil {
		return errors.New("installed but not authenticated — run: gh auth login")
	}
	return nil
}

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
			report("gh CLI", checkGH())
			return nil
		},
	})
}
