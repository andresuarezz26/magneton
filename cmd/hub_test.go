package cmd

import (
	"testing"

	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/paths"
)

func TestConfigFieldsRoundTrip(t *testing.T) {
	in := &config.Config{
		JiraBaseURL:          "https://x.atlassian.net",
		JiraEmail:            "me@x.com",
		JiraInProgressStatus: "En curso",
		Concurrency:          5,
		ModelPlan:            "claude-opus-4-8",
		ModelImpl:            "claude-sonnet-4-6",
		ModelReview:          "claude-haiku-4-5",
		Repos: []config.Repo{{
			Path: "/r", JQL: "q", Branch: "b", Compile: "c", Test: "t",
		}},
	}
	out := &config.Config{}
	applyConfigFields(out, configFields(in))

	if out.JiraBaseURL != in.JiraBaseURL || out.JiraEmail != in.JiraEmail ||
		out.JiraInProgressStatus != in.JiraInProgressStatus || out.Concurrency != 5 {
		t.Errorf("scalar round-trip failed: %+v", out)
	}
	if out.ModelPlan != "claude-opus-4-8" || out.ModelImpl != "claude-sonnet-4-6" || out.ModelReview != "claude-haiku-4-5" {
		t.Errorf("model round-trip failed: plan=%q impl=%q review=%q", out.ModelPlan, out.ModelImpl, out.ModelReview)
	}
	if len(out.Repos) != 1 || out.Repos[0].Path != "/r" || out.Repos[0].Compile != "c" {
		t.Errorf("repo round-trip failed: %+v", out.Repos)
	}
}

func TestMenuQuitIsLast(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	items := monitorModel{}.paletteItems()
	if items[len(items)-1].key != "quit" {
		t.Errorf("Quit should be last in the menu, got %q", items[len(items)-1].key)
	}
}

func TestConfigActionOpensForm(t *testing.T) {
	// doAction("config") with a saved config → form view with a non-nil 9-field form.
	t.Setenv("MAGNETON_HOME", t.TempDir())
	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(&config.Config{JiraBaseURL: "u", Concurrency: 3}); err != nil {
		t.Fatal(err)
	}
	mm, _ := monitorModel{}.doAction("config")
	hub := mm.(monitorModel)
	if hub.view != viewForm || hub.form == nil {
		t.Errorf("config should open a form view; view=%d form=%v", hub.view, hub.form)
	}
	if len(hub.form.fields) != 12 {
		t.Errorf("config form should have 12 fields, got %d", len(hub.form.fields))
	}
}
