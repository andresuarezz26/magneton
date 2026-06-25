package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/andresuarezz26/magneton/internal/paths"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	in := &Config{
		JiraBaseURL: "https://x.atlassian.net",
		JiraEmail:   "me@x.com",
		Concurrency: 4,
		Repos: []Repo{{
			Path:   "/repo",
			Branch: "ai/{ticket}-{slug}",
			Base:   "main",
		}},
	}
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.JiraBaseURL != in.JiraBaseURL || got.JiraEmail != in.JiraEmail || got.Concurrency != 4 {
		t.Errorf("scalars round-trip failed: %+v", got)
	}
	if len(got.Repos) != 1 || got.Repos[0].Path != "/repo" || got.Repos[0].Base != "main" {
		t.Errorf("repo round-trip failed: %+v", got.Repos)
	}
}

// parseSandbox unmarshals SandboxSettingsJSON into a struct we can assert on.
type sbWire struct {
	Sandbox struct {
		Enabled bool `json:"enabled"`
		Network *struct {
			AllowedDomains []string `json:"allowedDomains"`
		} `json:"network"`
		Filesystem *struct {
			AllowWrite []string `json:"allowWrite"`
		} `json:"filesystem"`
	} `json:"sandbox"`
}

func parseSandbox(t *testing.T, s string) sbWire {
	t.Helper()
	var w sbWire
	if err := json.Unmarshal([]byte(s), &w); err != nil {
		t.Fatalf("SandboxSettingsJSON is not valid JSON (%q): %v", s, err)
	}
	return w
}

// The default (zero) config disables the sandbox for magneton's runs and emits
// no network/filesystem allowlists.
func TestSandboxSettingsJSONDefaultDisabled(t *testing.T) {
	c := &Config{}
	w := parseSandbox(t, c.SandboxSettingsJSON())
	if w.Sandbox.Enabled {
		t.Error("default config should disable the sandbox for magneton runs")
	}
	if w.Sandbox.Network != nil || w.Sandbox.Filesystem != nil {
		t.Errorf("disabled sandbox should carry no allowlists, got %+v", w.Sandbox)
	}
}

// When enabled, the payload keeps the sandbox on and merges baked-in Gradle
// defaults with the user's extra domains/paths (with ~ expanded).
func TestSandboxSettingsJSONEnabledMergesDefaults(t *testing.T) {
	c := &Config{
		Sandbox: Sandbox{
			Enabled:        true,
			AllowedDomains: []string{"artifactory.example.com"},
			AllowWrite:     []string{"~/.m2"},
		},
	}
	w := parseSandbox(t, c.SandboxSettingsJSON())
	if !w.Sandbox.Enabled {
		t.Fatal("enabled config should keep the sandbox on")
	}
	if w.Sandbox.Network == nil || w.Sandbox.Filesystem == nil {
		t.Fatalf("enabled sandbox must carry allowlists, got %+v", w.Sandbox)
	}
	domains := strings.Join(w.Sandbox.Network.AllowedDomains, ",")
	for _, want := range []string{"repo.maven.apache.org", "dl.google.com", "artifactory.example.com"} {
		if !strings.Contains(domains, want) {
			t.Errorf("allowedDomains missing %q: %v", want, w.Sandbox.Network.AllowedDomains)
		}
	}
	writes := strings.Join(w.Sandbox.Filesystem.AllowWrite, ",")
	if !strings.Contains(writes, ".gradle") {
		t.Errorf("allowWrite missing the ~/.gradle default: %v", w.Sandbox.Filesystem.AllowWrite)
	}
	if strings.Contains(writes, "~/.m2") || !strings.Contains(writes, ".m2") {
		t.Errorf("user allow_write should be present and ~-expanded: %v", w.Sandbox.Filesystem.AllowWrite)
	}
}
