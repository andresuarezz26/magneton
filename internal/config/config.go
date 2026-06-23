// Package config loads ~/.agent/config.toml (Decision 15: global config keyed by repo path).
package config

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/andresuarezz26/magneton/internal/paths"
)

// Repo is one registered Android repository.
type Repo struct {
	Path          string `toml:"path"`
	Branch        string `toml:"branch"`
	Compile       string `toml:"compile"`
	Test          string `toml:"test"`
	ConnectedTest string `toml:"connected_test"`
	Base          string `toml:"base"`
	MaxRetries    int    `toml:"max_retries"`
}

// Config is the whole ~/.agent/config.toml.
type Config struct {
	JiraBaseURL  string  `toml:"jira_base_url"`
	JiraEmail    string  `toml:"jira_email"`
	PollInterval int     `toml:"poll_interval"`
	Concurrency          int     `toml:"concurrency"`
	AllowedTools         string  `toml:"allowed_tools"`
	MaxBudgetUSD         float64 `toml:"max_budget_usd"`
	ModelPlan            string  `toml:"model_plan"`
	ModelImpl            string  `toml:"model_impl"`
	ModelReview          string  `toml:"model_review"`
	AVDName              string  `toml:"avd_name"`
	AndroidSDKPath       string  `toml:"android_sdk_path"`
	EmulatorIdleTimeout  int     `toml:"emulator_idle_timeout"`
	TelemetryEnabled     *bool   `toml:"telemetry_enabled"`
	DeviceID             string  `toml:"device_id"`
	Repos                []Repo  `toml:"repo"`
}

// GenerateDeviceID returns a random UUID v4 string. Call once at consent time.
func GenerateDeviceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Save writes the config to ~/.agent/config.toml (0600), creating/truncating it.
// Same encoding the init wizard uses; reusable by the TUI config/setup forms.
func Save(c *Config) error {
	f, err := os.OpenFile(paths.Config(), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(c)
}

// Load reads and validates the config, applying defaults.
func Load() (*Config, error) {
	b, err := os.ReadFile(paths.Config())
	if err != nil {
		return nil, fmt.Errorf("read config (%s): %w — run `agent init`", paths.Config(), err)
	}
	var c Config
	if err := toml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.PollInterval == 0 {
		c.PollInterval = 30
	}
	if c.Concurrency == 0 {
		c.Concurrency = 3
	}
	if c.MaxBudgetUSD == 0 {
		c.MaxBudgetUSD = 5
	}
	if c.ModelPlan == "" {
		c.ModelPlan = "claude-haiku-4-5-20251001"
	}
	if c.ModelImpl == "" {
		c.ModelImpl = "claude-haiku-4-5-20251001"
	}
	if c.ModelReview == "" {
		c.ModelReview = "claude-haiku-4-5-20251001"
	}
	if c.AndroidSDKPath == "" {
		if v := os.Getenv("ANDROID_HOME"); v != "" {
			c.AndroidSDKPath = v
		} else {
			c.AndroidSDKPath = os.Getenv("ANDROID_SDK_ROOT")
		}
	}
	if c.EmulatorIdleTimeout == 0 {
		c.EmulatorIdleTimeout = 30
	}
	for i := range c.Repos {
		if c.Repos[i].ConnectedTest == "" {
			c.Repos[i].ConnectedTest = "./gradlew connectedDebugAndroidTest"
		}
	}
	if c.AllowedTools == "" {
		// Scoped allowlist (Decision 16): file edits within the worktree plus the
		// Gradle wrapper and read-only inspection commands — no arbitrary Bash, so
		// a misfire can't run destructive or network commands. Teams can widen
		// this in config if a repo needs more. (Note: read commands like `cat` can
		// still reach paths outside the worktree; full FS confinement needs a
		// sandbox, which is a later hardening step.)
		c.AllowedTools = "Edit Write Read Glob Grep MultiEdit TodoWrite " +
			"Bash(./gradlew:*) Bash(ls:*) Bash(cat:*) Bash(head:*) Bash(tail:*) " +
			"Bash(grep:*) Bash(rg:*) Bash(find:*) Bash(git status:*) " +
			"Bash(git diff:*) Bash(git log:*) Bash(git show:*)"
	}
	for i := range c.Repos {
		r := &c.Repos[i]
		r.Path = expand(r.Path)
		if r.MaxRetries == 0 {
			r.MaxRetries = 3
		}
		if r.Branch == "" {
			r.Branch = "ai/{ticket}-{slug}"
		}
	}
}

func expand(p string) string {
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// Expand resolves a leading ~ to the user's home directory.
func Expand(p string) string { return expand(p) }

// Resolve picks the repo to act on. Empty repoPath returns the first configured repo.
func (c *Config) Resolve(repoPath string) (*Repo, error) {
	if len(c.Repos) == 0 {
		return nil, fmt.Errorf("no [[repo]] configured in %s", paths.Config())
	}
	if repoPath == "" {
		return &c.Repos[0], nil
	}
	rp := expand(repoPath)
	for i := range c.Repos {
		if c.Repos[i].Path == rp {
			return &c.Repos[i], nil
		}
	}
	return nil, fmt.Errorf("repo %q not found in config", repoPath)
}
