package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/andresuarezz26/magneton/internal/config"
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

	// Branch pattern: {username} resolves to the git/GitHub user at run time.
	fmt.Println("\n  Branch names use variables: {username}, {ticket}, {slug} (title as kebab-case).")
	fmt.Println("  Example: for TICKET-1 \"Add pull to refresh\" → currentusername/ticket-1-add-pull-to-refresh")
	branchPattern := ask(r, "Branch pattern", "{username}/{ticket}-{slug}")

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
	prompt := "? " + label + ": "
	if def != "" {
		prompt = "? " + label + " [" + def + "]: "
	}
	line := readLine(r, prompt)
	if line == "" {
		return def
	}
	return line
}

// readLine reads one line with basic left/right cursor editing (←/→, Home/End,
// Backspace, Delete) by putting the terminal in raw mode for the read. Falls
// back to a plain buffered read when stdin is not an interactive terminal or
// raw mode can't be set. Any leading lines in prompt (before the last "\n") are
// printed as a static header so the editable prompt stays a single line.
func readLine(in *bufio.Reader, prompt string) string {
	static, edit := "", prompt
	if i := strings.LastIndex(prompt, "\n"); i >= 0 {
		static, edit = prompt[:i+1], prompt[i+1:]
	}
	if static != "" {
		fmt.Print(static)
	}

	fd := int(os.Stdin.Fd())
	oldState, err := (*term.State)(nil), error(nil)
	if term.IsTerminal(fd) {
		oldState, err = term.MakeRaw(fd)
	}
	if oldState == nil || err != nil {
		// Not a TTY (piped/redirected) or raw mode unavailable: plain read.
		fmt.Print(edit)
		line, _ := in.ReadString('\n')
		return strings.TrimSpace(line)
	}
	defer term.Restore(fd, oldState)

	line, abort := editLine(in, os.Stdout, edit)
	if abort {
		term.Restore(fd, oldState)
		os.Exit(130)
	}
	return line
}

// editLine runs the raw-mode line-editing loop: it reads runes from in, echoes
// the edited line to out, and returns the trimmed result. abort is true when the
// user pressed ctrl-c. It is independent of terminal setup so it can be tested
// with synthetic input.
func editLine(in io.RuneReader, out io.Writer, edit string) (result string, abort bool) {
	var buf []rune
	pos := 0
	redraw := func() {
		fmt.Fprint(out, "\r"+edit+string(buf)+"\x1b[K")
		if back := len(buf) - pos; back > 0 {
			fmt.Fprintf(out, "\x1b[%dD", back)
		}
	}
	redraw()
	for {
		rn, _, err := in.ReadRune()
		if err != nil {
			break
		}
		switch {
		case rn == '\r' || rn == '\n':
			fmt.Fprint(out, "\r\n")
			return strings.TrimSpace(string(buf)), false
		case rn == 3: // ctrl-c
			fmt.Fprint(out, "\r\n")
			return "", true
		case rn == 127 || rn == 8: // backspace
			if pos > 0 {
				buf = append(buf[:pos-1], buf[pos:]...)
				pos--
			}
		case rn == 27: // ESC: arrow/navigation sequence
			b1, _, err := in.ReadRune()
			if err != nil || (b1 != '[' && b1 != 'O') {
				break
			}
			b2, _, err := in.ReadRune()
			if err != nil {
				break
			}
			switch b2 {
			case 'D': // left
				if pos > 0 {
					pos--
				}
			case 'C': // right
				if pos < len(buf) {
					pos++
				}
			case 'H': // home
				pos = 0
			case 'F': // end
				pos = len(buf)
			case '3': // delete: ESC [ 3 ~
				if b3, _, err := in.ReadRune(); err == nil && b3 == '~' && pos < len(buf) {
					buf = append(buf[:pos], buf[pos+1:]...)
				}
			case '1', '7': // home variants: ESC [ 1 ~ / 7 ~
				if b3, _, err := in.ReadRune(); err == nil && b3 == '~' {
					pos = 0
				}
			case '4', '8': // end variants: ESC [ 4 ~ / 8 ~
				if b3, _, err := in.ReadRune(); err == nil && b3 == '~' {
					pos = len(buf)
				}
			}
		case rn >= 32: // printable rune: insert at cursor
			buf = append(buf, 0)
			copy(buf[pos+1:], buf[pos:])
			buf[pos] = rn
			pos++
		}
		redraw()
	}
	return strings.TrimSpace(string(buf)), false
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
	line := strings.ToLower(readLine(r, "? "+label+" "+suffix+": "))
	if line == "" {
		return def
	}
	return line == "y" || line == "yes"
}
