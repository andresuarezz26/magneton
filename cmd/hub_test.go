package cmd

import (
	"fmt"
	"strings"
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

// Enter on a content stack step advances to the final plan-review toggle step
// (no launch yet - the review step's Enter launches).
func TestStackEnterContentOpensReviewStep(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	m := stackModel("content")
	m.stackCursor = 0 // the "- none -" row
	nm, cmd := m.updateRunStack(tea.KeyMsg{Type: tea.KeyEnter})
	hub := nm.(monitorModel)
	if cmd != nil {
		t.Error("Enter on a content stack step should not launch yet - it opens the review step")
	}
	if hub.runReviewPrompt != 0 {
		t.Errorf("stack Enter should open the review step (runReviewPrompt=0), got %d", hub.runReviewPrompt)
	}
}

// The plan-review step launches on Enter for either choice (returns a non-nil
// command and consumes the pending tickets back to the dashboard).
func TestReviewStepEnterLaunches(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	for _, cursor := range []int{0, 1} {
		m := monitorModel{
			view:            viewRunInput,
			runMode:         "content",
			runTickets:      []pendingTicket{{id: "LOCAL-9", kind: "content", title: "t", lines: 1}},
			runReviewPrompt: 0,
			reviewCursor:    cursor,
		}
		nm, cmd := m.updateRunReview(tea.KeyMsg{Type: tea.KeyEnter})
		if cmd == nil {
			t.Errorf("cursor=%d: review-step Enter should launch (non-nil command)", cursor)
		}
		if nm.(monitorModel).view != viewDashboard {
			t.Errorf("cursor=%d: review-step Enter should return to dashboard", cursor)
		}
	}
}

// Esc in the review step cancels the whole creation (content ticket).
func TestReviewStepEscCancels(t *testing.T) {
	m := monitorModel{
		view:            viewRunInput,
		runMode:         "content",
		runTickets:      []pendingTicket{{id: "LOCAL-9", kind: "content", title: "t", lines: 1}},
		runReviewPrompt: 0,
	}
	nm, cmd := m.updateRunReview(tea.KeyMsg{Type: tea.KeyEsc})
	hub := nm.(monitorModel)
	if cmd != nil {
		t.Error("Esc in the review step must not launch")
	}
	if hub.view != viewDashboard || hub.runTickets != nil {
		t.Errorf("Esc should cancel creation: view=%d tickets=%+v", hub.view, hub.runTickets)
	}
}

// The review step's cursor is pre-selected from the config default: review_plans
// = true → cursor on "Yes" (0); unset → "No" (1).
func TestReviewPickerCursorFollowsConfig(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	base := monitorModel{
		runTickets: []pendingTicket{{id: "LOCAL-9", kind: "content", title: "t", lines: 1}},
	}

	if err := config.Save(&config.Config{ReviewPlans: true, Repos: []config.Repo{{Path: "/r"}}}); err != nil {
		t.Fatal(err)
	}
	nm, _ := base.openReviewPicker(0)
	if got := nm.(monitorModel).reviewCursor; got != 0 {
		t.Errorf("review_plans=true should pre-select Yes (0), got %d", got)
	}

	if err := config.Save(&config.Config{ReviewPlans: false, Repos: []config.Repo{{Path: "/r"}}}); err != nil {
		t.Fatal(err)
	}
	nm, _ = base.openReviewPicker(0)
	if got := nm.(monitorModel).reviewCursor; got != 1 {
		t.Errorf("review_plans=false should pre-select No (1), got %d", got)
	}
}

// renderBodyPreview: first lines appear verbatim in a quoted block; empty body
// renders nothing.
func TestRenderBodyPreview(t *testing.T) {
	// Empty body → "".
	if got := renderBodyPreview("", 80); got != "" {
		t.Errorf("empty body should render nothing, got %q", got)
	}
	if got := renderBodyPreview("   \n  ", 80); got != "" {
		t.Errorf("whitespace-only body should render nothing, got %q", got)
	}

	// A 3-line body: all three lines verbatim, no "+N more" suffix.
	out := renderBodyPreview("line one\nline two\nline three", 80)
	for _, want := range []string{"line one", "line two", "line three"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "more lines") {
		t.Errorf("3-line body should have no +N suffix:\n%s", out)
	}
}

// renderBodyPreview: a >12-line body shows the first 12 and a "+N more lines".
func TestRenderBodyPreviewMore(t *testing.T) {
	var lines []string
	for i := 1; i <= 20; i++ {
		lines = append(lines, fmt.Sprintf("row%d", i))
	}
	out := renderBodyPreview(strings.Join(lines, "\n"), 80)
	if !strings.Contains(out, "row1") || !strings.Contains(out, "row12") {
		t.Errorf("first 12 rows should show:\n%s", out)
	}
	if strings.Contains(out, "row13") {
		t.Errorf("13th row must be truncated (only 12 shown):\n%s", out)
	}
	if !strings.Contains(out, "+8 more lines") {
		t.Errorf("20 lines should show '+8 more lines':\n%s", out)
	}
}

// The config form round-trips the plan-review toggle as a loose y/n value.
func TestConfigFieldsReviewPlansYN(t *testing.T) {
	// ReviewPlans=true renders "y" and parses back to true.
	in := &config.Config{ReviewPlans: true, Repos: []config.Repo{{Path: "/r", Branch: "b"}}}
	fields := configFields(in)
	if fields[6].value != "y" {
		t.Errorf("review-plans field should render 'y', got %q", fields[6].value)
	}
	out := &config.Config{}
	applyConfigFields(out, fields)
	if !out.ReviewPlans {
		t.Error("y should parse back to ReviewPlans=true")
	}

	// ReviewPlans=false renders "n" and parses back to false; loose parsing.
	in.ReviewPlans = false
	fields = configFields(in)
	if fields[6].value != "n" {
		t.Errorf("review-plans field should render 'n', got %q", fields[6].value)
	}
	fields[6].value = "yes" // loose truthy
	applyConfigFields(out, fields)
	if !out.ReviewPlans {
		t.Error("'yes' should parse to true")
	}
	fields[6].value = "nope"
	applyConfigFields(out, fields)
	if out.ReviewPlans {
		t.Error("'nope' should parse to false")
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

func TestBaseLabel(t *testing.T) {
	sentinel := git.Branch{Name: defaultBaseSentinel}

	// Default row shows the resolved default branch name + "(default)".
	m := monitorModel{stackDefault: "main"}
	if got := m.baseLabel(sentinel); got != "main (default)" {
		t.Errorf("main default: got %q, want %q", got, "main (default)")
	}
	m.stackDefault = "develop"
	if got := m.baseLabel(sentinel); got != "develop (default)" {
		t.Errorf("develop default: got %q", got)
	}

	// Unknown default (couldn't resolve) → a clear fallback, never "none".
	m.stackDefault = ""
	if got := m.baseLabel(sentinel); got != "default base" {
		t.Errorf("empty default: got %q, want %q", got, "default base")
	}

	// A real branch row renders its own name unchanged.
	if got := m.baseLabel(git.Branch{Name: "ai/parent"}); got != "ai/parent" {
		t.Errorf("real branch: got %q", got)
	}
}

// Selecting the default row must leave base empty (use the repo default), even
// though it now displays a real branch name.
func TestStackEnterDefaultRowKeepsBaseEmpty(t *testing.T) {
	m := stackModel("jira")
	m.stackDefault = "main"
	m.stackCursor = 0 // the default row
	nm, _ := m.updateRunStack(tea.KeyMsg{Type: tea.KeyEnter})
	hub := nm.(monitorModel)
	if len(hub.runTickets) != 1 || hub.runTickets[0].base != "" {
		t.Errorf("default row should leave base empty, got %+v", hub.runTickets)
	}
}

func TestConfigFieldsRoundTrip(t *testing.T) {
	in := &config.Config{
		Concurrency: 5,
		ModelPlan:   "claude-opus-4-8",
		ModelImpl:   "claude-sonnet-4-6",
		ModelReview: "claude-haiku-4-5",
		Repos:       []config.Repo{{Path: "/r", Branch: "b", Base: "main"}},
	}
	out := &config.Config{}
	applyConfigFields(out, configFields(in))

	if out.ModelPlan != "claude-opus-4-8" || out.ModelImpl != "claude-sonnet-4-6" || out.ModelReview != "claude-haiku-4-5" {
		t.Errorf("model round-trip failed: plan=%q impl=%q review=%q", out.ModelPlan, out.ModelImpl, out.ModelReview)
	}
	if len(out.Repos) != 1 || out.Repos[0].Path != "/r" || out.Repos[0].Branch != "b" || out.Repos[0].Base != "main" {
		t.Errorf("repo round-trip failed: %+v", out.Repos)
	}
}

// Jira fields are no longer editable in the form, but any existing config value
// must be preserved (not wiped) when the form is saved.
func TestApplyConfigFieldsPreservesJira(t *testing.T) {
	cfg := &config.Config{
		JiraBaseURL: "https://x.atlassian.net",
		JiraEmail:   "me@x.com",
		Repos:       []config.Repo{{Path: "/r", Branch: "b"}},
	}
	applyConfigFields(cfg, configFields(cfg))
	if cfg.JiraBaseURL != "https://x.atlassian.net" || cfg.JiraEmail != "me@x.com" {
		t.Errorf("existing Jira config must be preserved, got base=%q email=%q", cfg.JiraBaseURL, cfg.JiraEmail)
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
	// doAction("config") with a saved config → form view with the 6 editable fields
	// (repo path, branch, base, and three models - no Jira).
	t.Setenv("MAGNETON_HOME", t.TempDir())
	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(&config.Config{Concurrency: 3}); err != nil {
		t.Fatal(err)
	}
	mm, _ := monitorModel{}.doAction("config")
	hub := mm.(monitorModel)
	if hub.view != viewForm || hub.form == nil {
		t.Errorf("config should open a form view; view=%d form=%v", hub.view, hub.form)
	}
	// 7 editable fields: repo path, branch, base, three models, and the plan-review
	// toggle (no Jira).
	if len(hub.form.fields) != 7 {
		t.Errorf("config form should have 7 fields, got %d", len(hub.form.fields))
	}
	for _, f := range hub.form.fields {
		if strings.Contains(f.label, "Jira") {
			t.Errorf("config form must not contain a Jira field: %q", f.label)
		}
	}
}
