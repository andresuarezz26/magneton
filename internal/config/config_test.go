package config

import (
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
			Path:    "/repo",
			Branch:  "ai/{ticket}-{slug}",
			Compile: "./gradlew compile",
			Test:    "./gradlew test",
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
	if len(got.Repos) != 1 || got.Repos[0].Path != "/repo" || got.Repos[0].Compile != "./gradlew compile" {
		t.Errorf("repo round-trip failed: %+v", got.Repos)
	}
}
