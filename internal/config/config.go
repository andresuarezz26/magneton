// Package config loads ~/.agent/config.toml (Decision 15: global config keyed by repo path).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/droidpilot/droidpilot/internal/paths"
)

// Repo is one registered Android repository.
type Repo struct {
	Path       string `toml:"path"`
	JQL        string `toml:"jql"`
	Branch     string `toml:"branch"`
	Compile    string `toml:"compile"`
	Test       string `toml:"test"`
	Base       string `toml:"base"`
	MaxRetries int    `toml:"max_retries"`
}

// Config is the whole ~/.agent/config.toml.
type Config struct {
	JiraBaseURL  string  `toml:"jira_base_url"`
	JiraEmail    string  `toml:"jira_email"`
	PollInterval int     `toml:"poll_interval"`
	Concurrency  int     `toml:"concurrency"`
	AllowedTools string  `toml:"allowed_tools"`
	MaxBudgetUSD float64 `toml:"max_budget_usd"`
	Repos        []Repo  `toml:"repo"`
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
	if c.AllowedTools == "" {
		// Scoped allowlist (Decision 16). Writes are further confined to the
		// worktree because the session's cwd is the worktree. Bash is broad for
		// the MVP; tighten to Bash(./gradlew:*) etc. once flows stabilize.
		c.AllowedTools = "Edit Write Read Glob Grep MultiEdit Bash TodoWrite"
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
