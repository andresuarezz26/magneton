package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/git"
	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/secrets"
	"github.com/andresuarezz26/magneton/internal/vcs"
)

// Non-interactive fallback (CI / piped stdin): a commented config to edit by hand.
const sampleConfig = `# magneton config - ~/.magneton/config.toml
poll_interval = 30
concurrency   = 3
max_budget_usd = 5
# telemetry_enabled = false   # set to true to share anonymous usage data
# allowed_tools = "Edit Write Read Glob Grep MultiEdit Bash TodoWrite"
# Per-stage models - blank/omitted = use Claude Code's configured default.
# Set any identifier your account allows (alias or full name).
# model_plan   = "haiku"
# model_impl   = "sonnet"
# model_review = "opus"

# Claude Code OS sandbox posture for magneton's own runs. By default magneton
# DISABLES the sandbox for its child claude, because Gradle needs network access
# and writes to ~/.gradle that the sandbox blocks (magneton's guardrail is the
# scoped allowed_tools allowlist, not the OS sandbox). To keep the sandbox on -
# e.g. on a shared/CI machine - set enabled = true; magneton then bakes in the
# domains/paths Gradle needs, and you can add more here.
# [sandbox]
# enabled = true
# allowed_domains = ["your-artifactory.example.com"]
# allow_write     = ["~/.m2"]

[[repo]]
path        = "~/src/android-app"
branch      = "{username}/{ticket}-{slug}"
# base      = "main"
# Build/test commands are intentionally not configured: the agent discovers and
# runs verification itself (handles per-project setups and company build skills).
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
		fmt.Printf("✓ wrote %s - edit it for your repo\n", cfgPath)
	} else {
		fmt.Printf("• config already exists at %s\n", cfgPath)
	}
	fmt.Println("✓ templates in", paths.Templates())
	fmt.Println("\nNext: run `gh auth login`, then `magneton run ./ticket.md --dry-run`.")
	return nil
}

func wizard() error {
	r := bufio.NewReader(os.Stdin)
	cfgPath := paths.Config()
	if _, err := os.Stat(cfgPath); err == nil {
		if !askYesNo(r, "config exists at "+cfgPath+" - overwrite?", false) {
			fmt.Println("aborted; existing config left untouched")
			return nil
		}
	}

	fmt.Println("\nmagneton setup\n────────────────")
	cfg := config.Config{PollInterval: 30, Concurrency: 3, MaxBudgetUSD: 5}

	// Required: repo path.
	fmt.Println("\n  Tip: cd to your Android project in Terminal, then run: pwd")
	repoPath := ask(r, "Repository path", "~/src/android-app")
	if expanded := config.Expand(repoPath); expanded != "" {
		if _, err := os.Stat(expanded); os.IsNotExist(err) {
			fmt.Println("  (warn) that path doesn't exist yet — update it in", paths.Config(), "before running magneton")
		}
	}

	// Branch pattern: default uses the developer's GitHub username.
	username := git.ResolveUsername()
	defaultBranch := username + "/{ticket}-{slug}"
	fmt.Println("\n  Branch names use variables: {username}, {ticket}, {slug} (title as kebab-case).")
	fmt.Printf("  Example: for TICKET-1 \"Add pull to refresh\" → %s/ticket-1-add-pull-to-refresh\n", username)
	branchPattern := ask(r, "Branch pattern", defaultBranch)

	repo := config.Repo{Path: repoPath, Branch: branchPattern}
	cfg.Repos = []config.Repo{repo}

	// Optional: per-stage models. Blank inherits whatever default Claude Code is
	// configured with (respecting org policy), so most users can skip these.
	fmt.Println("\n  - Models per stage [optional] ---------------------")
	fmt.Println("  Blank = use Claude Code's default. Enter any identifier your")
	fmt.Println("  account allows (e.g. haiku, sonnet, opus, or a full name).")
	cfg.ModelPlan = ask(r, "Model · plan [optional]", "")
	cfg.ModelImpl = ask(r, "Model · implement [optional]", "")
	cfg.ModelReview = ask(r, "Model · review [optional]", "")

	// Optional: Anthropic key (most users rely on the logged-in claude session).
	if tok := askSecret("Anthropic API key [optional - blank = use logged-in claude]"); tok != "" {
		_ = secrets.Set(secrets.Anthropic, tok)
		fmt.Println("  → saved to OS keychain")
	}

	// Telemetry consent.
	fmt.Println()
	telEnabled := askYesNo(r, "Share anonymous usage data to help improve magneton?\n  (OS type, run outcome, duration - never ticket content or file paths)", true)
	cfg.TelemetryEnabled = &telEnabled
	if telEnabled {
		cfg.DeviceID = config.GenerateDeviceID()
	}

	// Write config.
	if err := config.Save(&cfg); err != nil {
		return err
	}
	fmt.Printf("\n✓ wrote %s\n", cfgPath)

	// Connectivity check.
	fmt.Println("\nconnectivity check")
	report("git remote (origin)", checkGitRemote(config.Expand(repo.Path)))
	report("claude CLI", exec.Command("claude", "--version").Run())
	report("gh CLI", exec.Command("gh", "auth", "status").Run())

	fmt.Println("\nReady. Try:  magneton run ./ticket.md --dry-run")
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
