package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/droidpilot/droidpilot/internal/paths"
	"github.com/droidpilot/droidpilot/internal/vcs"
)

// Phase 1 ships a non-interactive scaffold of init. The full interactive wizard
// with a connectivity check is Decision 13 (Phase 3).
const sampleConfig = `# droidpilot config — ~/.agent/config.toml
jira_base_url = "https://your-org.atlassian.net"
jira_email    = "you@your-org.com"
poll_interval = 30
concurrency   = 3
max_budget_usd = 5
# allowed_tools = "Edit Write Read Glob Grep MultiEdit Bash TodoWrite"

[[repo]]
path        = "~/src/android-app"
jql         = "labels = ai-agent AND status = 'To Do'"
branch      = "ai/{ticket}-{slug}"
compile     = "./gradlew :app:compileDebug"
test        = "./gradlew testDebugUnitTest"
max_retries = 3
# base      = "main"
`

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "Scaffold ~/.agent config + templates",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := paths.EnsureDirs(); err != nil {
				return err
			}
			if err := vcs.WriteDefaultTemplates(); err != nil {
				return err
			}
			cfgPath := paths.Config()
			if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
				if err := os.WriteFile(cfgPath, []byte(sampleConfig), 0o600); err != nil {
					return err
				}
				fmt.Printf("✓ wrote %s — edit it for your repo\n", cfgPath)
			} else {
				fmt.Printf("• config already exists at %s\n", cfgPath)
			}
			fmt.Println("✓ templates in", paths.Templates())
			fmt.Println()
			fmt.Println("Next:")
			fmt.Println("  export DROIDPILOT_JIRA_TOKEN=...   # or stored in the OS keychain")
			fmt.Println("  gh auth login                      # for opening PRs")
			fmt.Println("  agent run <TICKET>")
			return nil
		},
	})
}
