package cmd

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/git"
	"github.com/andresuarezz26/magneton/internal/paths"
)

// stackModel builds a monitorModel parked on the stack-picker sub-step for one
// pending ticket of the given kind, with a single selectable branch.
func stackModel(kind string) monitorModel {
	return monitorModel{
		view:           viewRunInput,
		runMode:        kind,
		runTickets:     []pendingTicket{{id: "LOCAL-9", kind: kind, title: "t", lines: 1}},
		runStackPrompt: 0,
		stackBranches:  []git.Branch{{Name: "ai/parent"}},
	}
}

// Regression: pressing Esc in the stack picker must CANCEL the creation, never
// launch the ticket. Previously Esc on a content ticket auto-launched it.
func TestStackEscCancelsContentDoesNotLaunch(t *testing.T) {
	m := stackModel("content")
	nm, cmd := m.updateRunStack(tea.KeyMsg{Type: tea.KeyEsc})
	hub := nm.(monitorModel)
	if cmd != nil {
		t.Error("Esc on a content stack step returned a command - it must not launch")
	}
	if hub.view != viewDashboard {
		t.Errorf("Esc should return to the dashboard, got view=%d", hub.view)
	}
	if hub.runTickets != nil {
		t.Errorf("Esc should clear pending tickets, got %+v", hub.runTickets)
	}
	if hub.runStackPrompt != -1 {
		t.Errorf("Esc should reset runStackPrompt to -1, got %d", hub.runStackPrompt)
	}
}

// Enter on a content stack step is the last finalize action → it launches
// (returns a non-nil command).
func TestStackEnterContentLaunches(t *testing.T) {
	m := stackModel("content")
	m.stackCursor = 0 // the "- none -" row
	_, cmd := m.updateRunStack(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Error("Enter on a content stack step should launch (non-nil command)")
	}
}

// Enter selecting a real branch records it on the chip. Using a jira chip so the
// picker just closes (no launch) and we can inspect the resulting base.
func TestStackEnterSetsBase(t *testing.T) {
	m := stackModel("jira")
	m.stackCursor = 1 // list = [none, ai/parent] → pick ai/parent
	nm, cmd := m.updateRunStack(tea.KeyMsg{Type: tea.KeyEnter})
	hub := nm.(monitorModel)
	if cmd != nil {
		t.Error("Enter on a jira stack step should not launch")
	}
	if len(hub.runTickets) != 1 || hub.runTickets[0].base != "ai/parent" {
		t.Errorf("expected base ai/parent, got %+v", hub.runTickets)
	}
	if hub.runStackPrompt != -1 {
		t.Errorf("picker should close (runStackPrompt=-1), got %d", hub.runStackPrompt)
	}
}

// Enter on the "- none -" row leaves the base empty (default) and does not launch.
func TestStackEnterNoneKeepsDefault(t *testing.T) {
	m := stackModel("jira")
	m.stackCursor = 0 // the "- none -" row
	nm, _ := m.updateRunStack(tea.KeyMsg{Type: tea.KeyEnter})
	hub := nm.(monitorModel)
	if len(hub.runTickets) != 1 || hub.runTickets[0].base != "" {
		t.Errorf("none row should leave base empty, got %+v", hub.runTickets)
	}
}

// Esc on a jira/file stack step (opened via ctrl+s over a chip list) only closes
// the picker; it must NOT cancel the whole batch or drop the chips.
func TestStackEscJiraKeepsChips(t *testing.T) {
	m := stackModel("jira")
	nm, cmd := m.updateRunStack(tea.KeyMsg{Type: tea.KeyEsc})
	hub := nm.(monitorModel)
	if cmd != nil {
		t.Error("Esc on a jira stack step should not launch")
	}
	if hub.view != viewRunInput {
		t.Errorf("Esc on jira should stay in run-input, got view=%d", hub.view)
	}
	if len(hub.runTickets) != 1 {
		t.Errorf("Esc on jira should keep the chips, got %+v", hub.runTickets)
	}
	if hub.runStackPrompt != -1 {
		t.Errorf("picker should close (runStackPrompt=-1), got %d", hub.runStackPrompt)
	}
}

func TestConfigFieldsRoundTrip(t *testing.T) {
	in := &config.Config{
		JiraBaseURL: "https://x.atlassian.net",
		JiraEmail:   "me@x.com",
		Concurrency: 5,
		ModelPlan:   "claude-opus-4-8",
		ModelImpl:   "claude-sonnet-4-6",
		ModelReview: "claude-haiku-4-5",
		Repos:       []config.Repo{{Path: "/r", Branch: "b"}},
	}
	out := &config.Config{}
	applyConfigFields(out, configFields(in))

	if out.JiraBaseURL != in.JiraBaseURL || out.JiraEmail != in.JiraEmail {
		t.Errorf("scalar round-trip failed: %+v", out)
	}
	if out.ModelPlan != "claude-opus-4-8" || out.ModelImpl != "claude-sonnet-4-6" || out.ModelReview != "claude-haiku-4-5" {
		t.Errorf("model round-trip failed: plan=%q impl=%q review=%q", out.ModelPlan, out.ModelImpl, out.ModelReview)
	}
	if len(out.Repos) != 1 || out.Repos[0].Path != "/r" || out.Repos[0].Branch != "b" {
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
	// doAction("config") with a saved config → form view with a non-nil 7-field form.
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
	if len(hub.form.fields) != 8 {
		t.Errorf("config form should have 8 fields, got %d", len(hub.form.fields))
	}
}
