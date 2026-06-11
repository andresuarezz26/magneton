package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/jira"
	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/secrets"
	"github.com/andresuarezz26/magneton/internal/vcs"
)

// Non-interactive fallback (CI / piped stdin): a commented config to edit by hand.
const sampleConfig = `# magneton config — ~/.agent/config.toml
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
		Short: "Set up magneton (interactive wizard; scaffolds a config when non-interactive)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := paths.EnsureDirs(); err != nil {
				return err
			}
			if err := vcs.WriteDefaultTemplates(); err != nil {
				return err
			}
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return scaffoldConfig()
			}
			return wizard()
		},
	})
}

func scaffoldConfig() error {
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
	fmt.Println("\nNext: set MAGNETON_JIRA_TOKEN, run `gh auth login`, then `agent run <TICKET>`.")
	return nil
}

func wizard() error {
	r := bufio.NewReader(os.Stdin)
	cfgPath := paths.Config()
	if _, err := os.Stat(cfgPath); err == nil {
		if !askYesNo(r, "config exists at "+cfgPath+" — overwrite?", false) {
			fmt.Println("aborted; existing config left untouched")
			return nil
		}
	}

	fmt.Println("\nmagneton setup\n────────────────")
	cfg := config.Config{PollInterval: 30, Concurrency: 3, MaxBudgetUSD: 5}
	cfg.JiraBaseURL = strings.TrimRight(ask(r, "Jira base URL", "https://your-org.atlassian.net"), "/")
	cfg.JiraEmail = ask(r, "Jira email", "")
	repo := config.Repo{
		Path:       ask(r, "Repository path", "~/src/android-app"),
		JQL:        ask(r, "JQL filter", "labels = ai-agent AND status = 'To Do'"),
		Branch:     ask(r, "Branch pattern", "ai/{ticket}-{slug}"),
		Compile:    ask(r, "Compile command", "./gradlew :app:compileDebug"),
		Test:       ask(r, "Test command", "./gradlew testDebugUnitTest"),
		MaxRetries: 3,
	}
	cfg.Repos = []config.Repo{repo}

	// Secrets → OS keychain (Decision 14).
	if tok := askSecret("Jira API token"); tok != "" {
		if err := secrets.Set(secrets.Jira, tok); err != nil {
			fmt.Println("  (warn) could not store Jira token in keychain:", err)
		} else {
			fmt.Println("  → saved to OS keychain")
		}
	}
	if tok := askSecret("Anthropic API key (blank = use logged-in claude)"); tok != "" {
		_ = secrets.Set(secrets.Anthropic, tok)
		fmt.Println("  → saved to OS keychain")
	}

	// Write config.
	f, err := os.OpenFile(cfgPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(cfg); err != nil {
		f.Close()
		return err
	}
	f.Close()
	fmt.Printf("\n✓ wrote %s\n", cfgPath)

	// Connectivity check (Decision 13).
	fmt.Println("\nconnectivity check")
	jc := jira.New(cfg.JiraBaseURL, cfg.JiraEmail, secrets.Get(secrets.Jira))
	report("Jira", jc.Verify())
	report("git remote (origin)", checkGitRemote(config.Expand(repo.Path)))
	report("claude CLI", exec.Command("claude", "--version").Run())
	report("gh CLI", exec.Command("gh", "auth", "status").Run())

	fmt.Println("\nReady. Try:  agent run <TICKET> --dry-run")
	return nil
}

func report(label string, err error) {
	if err != nil {
		fmt.Printf("  ✗ %s: %v\n", label, err)
		return
	}
	fmt.Printf("  ✓ %s\n", label)
}

func checkGitRemote(repoPath string) error {
	out, err := exec.Command("git", "-C", repoPath, "remote", "get-url", "origin").CombinedOutput()
	if err != nil {
		return fmt.Errorf("no origin remote (%s)", strings.TrimSpace(string(out)))
	}
	return nil
}

func ask(r *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Printf("? %s [%s]: ", label, def)
	} else {
		fmt.Printf("? %s: ", label)
	}
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func askSecret(label string) string {
	fmt.Printf("? %s (hidden): ", label)
	b, _ := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	return strings.TrimSpace(string(b))
}

func askYesNo(r *bufio.Reader, label string, def bool) bool {
	suffix := "[y/N]"
	if def {
		suffix = "[Y/n]"
	}
	fmt.Printf("? %s %s: ", label, suffix)
	line, _ := r.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return def
	}
	return line == "y" || line == "yes"
}
