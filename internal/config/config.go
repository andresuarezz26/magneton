// Package config loads ~/.agent/config.toml (Decision 15: global config keyed by repo path).
package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/andresuarezz26/magneton/internal/paths"
)

// Repo is one registered Android repository. magneton no longer stores
// build/test commands here - the agent discovers and runs verification itself
// during the verify stage.
type Repo struct {
	Path   string `toml:"path"`
	Branch string `toml:"branch"`
	Base   string `toml:"base"`
}

// Config is the whole ~/.agent/config.toml.
type Config struct {
	JiraBaseURL         string  `toml:"jira_base_url"`
	JiraEmail           string  `toml:"jira_email"`
	PollInterval        int     `toml:"poll_interval"`
	Concurrency         int     `toml:"concurrency"`
	AllowedTools        string  `toml:"allowed_tools"`
	MaxBudgetUSD        float64 `toml:"max_budget_usd"`
	ModelPlan           string  `toml:"model_plan"`
	ModelImpl           string  `toml:"model_impl"`
	ModelReview         string  `toml:"model_review"`
	AVDName             string  `toml:"avd_name"`
	AndroidSDKPath      string  `toml:"android_sdk_path"`
	EmulatorIdleTimeout int     `toml:"emulator_idle_timeout"`
	TelemetryEnabled    *bool   `toml:"telemetry_enabled"`
	DeviceID            string  `toml:"device_id"`
	Sandbox             Sandbox `toml:"sandbox"`
	Repos               []Repo  `toml:"repo"`
}

// Sandbox controls Claude Code's OS sandbox for magneton's child `claude` runs.
// The default (Enabled=false) disables the sandbox for magneton's autonomous
// runs so Gradle gets the network and `~/.gradle` writes it needs - magneton's
// guardrail is the scoped --allowed-tools allowlist, not the OS sandbox. Set
// Enabled=true (e.g. on shared/CI machines) to keep the sandbox on; the
// Gradle-friendly defaults below are then merged with these extra lists.
type Sandbox struct {
	Enabled        bool     `toml:"enabled"`
	AllowedDomains []string `toml:"allowed_domains"` // extra network domains when Enabled
	AllowWrite     []string `toml:"allow_write"`     // extra writable paths when Enabled
}

// defaultSandboxDomains / defaultSandboxWrites are the network + filesystem
// allowances Gradle needs, baked in so an Enabled sandbox can still build a
// typical Android project. Users extend these via [sandbox] in config.
var (
	defaultSandboxDomains = []string{
		"repo.maven.apache.org", // Maven Central
		"dl.google.com",         // Google Maven (AndroidX, build tools)
		"*.gradle.org",          // plugins.gradle.org, services.gradle.org, …
	}
	defaultSandboxWrites = []string{"~/.gradle", "~/.konan", "~/.android"}
)

// SandboxSettingsJSON returns the `--settings` payload magneton passes to its
// child `claude`, overriding the machine's global sandbox setting for that one
// process. Default (Enabled=false) → the sandbox is turned off for magneton's
// runs. Enabled=true → the sandbox stays on with Gradle-friendly allowlists.
func (c *Config) SandboxSettingsJSON() string {
	type network struct {
		AllowedDomains []string `json:"allowedDomains,omitempty"`
	}
	type filesystem struct {
		AllowWrite []string `json:"allowWrite,omitempty"`
	}
	type sandbox struct {
		Enabled    bool        `json:"enabled"`
		Network    *network    `json:"network,omitempty"`
		Filesystem *filesystem `json:"filesystem,omitempty"`
	}
	s := sandbox{Enabled: c.Sandbox.Enabled}
	if c.Sandbox.Enabled {
		domains := append(append([]string{}, defaultSandboxDomains...), c.Sandbox.AllowedDomains...)
		writes := append(append([]string{}, defaultSandboxWrites...), c.Sandbox.AllowWrite...)
		if c.AndroidSDKPath != "" {
			writes = append(writes, c.AndroidSDKPath)
		}
		for i, w := range writes {
			writes[i] = expand(w)
		}
		s.Network = &network{AllowedDomains: domains}
		s.Filesystem = &filesystem{AllowWrite: writes}
	}
	b, err := json.Marshal(struct {
		Sandbox sandbox `json:"sandbox"`
	}{s})
	if err != nil {
		// Static shapes; marshal can't realistically fail. Fall back to the
		// disabled posture rather than emitting nothing.
		return `{"sandbox":{"enabled":false}}`
	}
	return string(b)
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
		return nil, fmt.Errorf("read config (%s): %w - run `agent init`", paths.Config(), err)
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
	// Models are intentionally left empty by default: an empty value makes the
	// agent omit `--model`, so Claude Code uses whatever default the user/org has
	// configured (via `claude`'s /model, respecting enterprise policy). Users can
	// set per-stage overrides to any identifier their account allows (including
	// non-Claude models on Bedrock/Vertex). Nothing is hardcoded here.
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
	if c.AllowedTools == "" {
		// Scoped allowlist (Decision 16): file edits within the worktree plus the
		// Gradle wrapper and read-only inspection commands - no arbitrary Bash, so
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
		if r.Branch == "" {
			r.Branch = "{username}/{ticket}-{slug}"
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
