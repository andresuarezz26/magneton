package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// Pasting content opens the id prompt (step 1) pre-filled with the detected
// id; confirming it computes the branch pre-fill from the CONFIRMED id and
// opens the branch prompt (step 2).
func TestContentFlowIDThenBranch(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(&config.Config{
		Repos: []config.Repo{{Path: "/r", Branch: "magneton/{ticket}"}},
	}); err != nil {
		t.Fatal(err)
	}
	m := monitorModel{
		view: viewRunInput, runMode: "content",
		runIDPrompt: -1, runBranchPrompt: -1, runImgPrompt: -1,
	}
	nm, _ := m.addContentTicket("PROJ-123 Fix the crash\nmore detail")
	hub := nm.(monitorModel)
	if len(hub.runTickets) != 1 {
		t.Fatalf("expected 1 chip, got %+v", hub.runTickets)
	}
	if hub.runIDPrompt != 0 || hub.runBranchPrompt != -1 {
		t.Fatalf("paste should open the id prompt first: idPrompt=%d branchPrompt=%d",
			hub.runIDPrompt, hub.runBranchPrompt)
	}
	if hub.runTickets[0].id != "PROJ-123" {
		t.Errorf("detected id should pre-fill, got %q", hub.runTickets[0].id)
	}
	if hub.runTickets[0].branch != "" {
		t.Errorf("branch must not be set before the id is confirmed, got %q", hub.runTickets[0].branch)
	}

	// The user fixes the id, then confirms: the branch pre-fill must use the
	// EDITED id, and the flow advances to the branch prompt.
	hub.runTickets[0].id = "PROJ-777"
	nm, _ = hub.updateRunIDPrompt(tea.KeyMsg{Type: tea.KeyEnter})
	hub = nm.(monitorModel)
	if hub.runIDPrompt != -1 || hub.runBranchPrompt != 0 {
		t.Errorf("id Enter should advance to the branch prompt: idPrompt=%d branchPrompt=%d",
			hub.runIDPrompt, hub.runBranchPrompt)
	}
	if got := hub.runTickets[0].branch; got != "magneton/proj-777" {
		t.Errorf("branch pre-fill should use the confirmed id: got %q, want %q", got, "magneton/proj-777")
	}
}

// Content mode is a real multi-line editor: Enter inserts a NEW LINE (it never
// creates a chip), and ctrl+d confirms the composed markdown into a chip that
// enters the id-confirm step, resetting the editor.
func TestContentEditorEnterIsNewlineCtrlDConfirms(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	m := monitorModel{
		view: viewRunInput, runMode: "content",
		runIDPrompt: -1, runBranchPrompt: -1, runImgPrompt: -1,
	}.withContentEditor()
	step := func(msg tea.KeyMsg) {
		nm, _ := m.updateRunContent(msg)
		m = nm.(monitorModel)
	}
	step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("PROJ-5 Fix the crash")})
	step(tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.runTickets) != 0 {
		t.Fatalf("Enter must insert a newline, not create a chip: %+v", m.runTickets)
	}
	step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("More detail")})
	if got := m.content.Value(); !strings.Contains(got, "\n") {
		t.Fatalf("editor should be multi-line after Enter, got %q", got)
	}
	step(tea.KeyMsg{Type: tea.KeyCtrlD})
	if len(m.runTickets) != 1 || m.runTickets[0].kind != "content" {
		t.Fatalf("ctrl+d should create a content chip, got %+v", m.runTickets)
	}
	if m.runTickets[0].id != "PROJ-5" || m.runIDPrompt != 0 {
		t.Errorf("chip should enter the id-confirm step: id=%q idPrompt=%d",
			m.runTickets[0].id, m.runIDPrompt)
	}
	if m.runTickets[0].lines != 2 {
		t.Errorf("chip should carry both lines, got %d", m.runTickets[0].lines)
	}
	if m.content.Value() != "" {
		t.Errorf("editor should reset after confirm, got %q", m.content.Value())
	}
}

// Esc in the editor steps out to the action bar (it must NOT cancel and
// destroy the draft); Enter on Continue confirms the chip; Esc on the bar
// returns to editing; Discard is the only way Esc-ish input drops the draft.
func TestContentEditorEscFocusesActionBar(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	m := monitorModel{
		view: viewRunInput, runMode: "content",
		runIDPrompt: -1, runBranchPrompt: -1, runImgPrompt: -1,
	}.withContentEditor()
	step := func(msg tea.KeyMsg) {
		nm, _ := m.updateRunContent(msg)
		m = nm.(monitorModel)
	}
	step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("PROJ-8 A draft")})
	step(tea.KeyMsg{Type: tea.KeyEsc})
	if m.view != viewRunInput || m.content == nil || m.content.Value() != "PROJ-8 A draft" {
		t.Fatal("Esc must keep the draft and stay on the screen")
	}
	if !m.contentBar || m.contentBtn != 0 {
		t.Fatalf("Esc should focus the action bar on Continue: bar=%v btn=%d", m.contentBar, m.contentBtn)
	}

	// Esc on the bar → back to editing, draft intact.
	step(tea.KeyMsg{Type: tea.KeyEsc})
	if m.contentBar || m.content.Value() != "PROJ-8 A draft" {
		t.Fatal("Esc on the bar should return to editing with the draft intact")
	}

	// Esc → Enter (Continue) confirms the ticket into the id step.
	step(tea.KeyMsg{Type: tea.KeyEsc})
	step(tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.runTickets) != 1 || m.runTickets[0].id != "PROJ-8" || m.runIDPrompt != 0 {
		t.Fatalf("Continue should confirm the chip into the id step, got %+v idPrompt=%d",
			m.runTickets, m.runIDPrompt)
	}
}

// Discard on the action bar cancels the whole creation.
func TestContentEditorDiscard(t *testing.T) {
	m := monitorModel{
		view: viewRunInput, runMode: "content",
		runIDPrompt: -1, runBranchPrompt: -1, runImgPrompt: -1,
	}.withContentEditor()
	step := func(msg tea.KeyMsg) {
		nm, _ := m.updateRunContent(msg)
		m = nm.(monitorModel)
	}
	step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("scrap this")})
	step(tea.KeyMsg{Type: tea.KeyEsc})
	step(tea.KeyMsg{Type: tea.KeyRight})
	step(tea.KeyMsg{Type: tea.KeyRight})
	if m.contentBtn != 2 {
		t.Fatalf("→→ should land on Discard, got btn=%d", m.contentBtn)
	}
	step(tea.KeyMsg{Type: tea.KeyEnter})
	if m.view != viewDashboard || m.runTickets != nil || m.content != nil {
		t.Errorf("Discard should cancel the creation: view=%d tickets=%+v", m.view, m.runTickets)
	}
}

// A paste lands IN the editor (for review/editing) instead of instantly
// becoming a chip, and ctrl+p toggles the markdown preview; esc in the preview
// returns to editing rather than cancelling the creation.
func TestContentEditorPasteAndPreview(t *testing.T) {
	m := monitorModel{
		view: viewRunInput, runMode: "content",
		runIDPrompt: -1, runBranchPrompt: -1, runImgPrompt: -1,
	}.withContentEditor()
	step := func(msg tea.KeyMsg) {
		nm, _ := m.updateRunContent(msg)
		m = nm.(monitorModel)
	}
	step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("# Title\rpasted body"), Paste: true})
	if len(m.runTickets) != 0 {
		t.Fatalf("paste must fill the editor, not create a chip: %+v", m.runTickets)
	}
	if got := m.content.Value(); got != "# Title\npasted body" {
		t.Fatalf("paste should land in the editor (\\r normalized), got %q", got)
	}
	step(tea.KeyMsg{Type: tea.KeyCtrlP})
	if !m.contentPreview || len(m.ticketLines) == 0 {
		t.Fatalf("ctrl+p should open the rendered preview: preview=%v lines=%d",
			m.contentPreview, len(m.ticketLines))
	}
	step(tea.KeyMsg{Type: tea.KeyEsc})
	if m.contentPreview {
		t.Error("esc in preview should return to editing")
	}
	if m.view != viewRunInput || m.content == nil {
		t.Error("esc in preview must not cancel the creation")
	}
}

// Enter on an emptied id field must not advance - the id is required (it names
// the dashboard row, worktree, and logs).
func TestIDPromptEnterRejectsEmpty(t *testing.T) {
	m := monitorModel{
		view: viewRunInput, runMode: "content",
		runTickets:  []pendingTicket{{kind: "content", title: "t", lines: 1, id: ""}},
		runIDPrompt: 0, runBranchPrompt: -1, runImgPrompt: -1,
	}
	nm, _ := m.updateRunIDPrompt(tea.KeyMsg{Type: tea.KeyEnter})
	hub := nm.(monitorModel)
	if hub.runIDPrompt != 0 || hub.runBranchPrompt != -1 {
		t.Errorf("empty id should keep the prompt open: idPrompt=%d branchPrompt=%d",
			hub.runIDPrompt, hub.runBranchPrompt)
	}
}

// Esc on the id prompt drops the chip entirely.
func TestIDPromptEscDropsChip(t *testing.T) {
	m := monitorModel{
		view: viewRunInput, runMode: "content",
		runTickets:  []pendingTicket{{kind: "content", title: "t", lines: 1, id: "LOCAL-9"}},
		runIDPrompt: 0, runBranchPrompt: -1, runImgPrompt: -1,
	}
	nm, _ := m.updateRunIDPrompt(tea.KeyMsg{Type: tea.KeyEsc})
	hub := nm.(monitorModel)
	if len(hub.runTickets) != 0 || hub.runIDPrompt != -1 {
		t.Errorf("Esc should drop the chip: tickets=%+v idPrompt=%d",
			hub.runTickets, hub.runIDPrompt)
	}
}

// No detectable ticket id → the pattern expands with the PASTED fallback id
// (matching writePastedTicket), never an empty field.
func TestInferBranchNoIDFallsBack(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	if got := inferBranch("", "Fix the crash"); got != "pasted-fix-the-crash" {
		t.Errorf("inferBranch fallback: got %q, want %q", got, "pasted-fix-the-crash")
	}
}

// Enter on the branch prompt confirms the (possibly edited) name - collapsing
// whitespace, which git forbids in branch names - and advances to the image
// step. The confirmed value is final; it is passed through as --branch.
func TestBranchPromptEnterAdvances(t *testing.T) {
	m := monitorModel{
		view: viewRunInput, runMode: "content",
		runTickets:      []pendingTicket{{id: "LOCAL-9", kind: "content", title: "t", lines: 1, branch: " magneton/local-9  fix "}},
		runBranchPrompt: 0, runImgPrompt: -1,
	}
	nm, _ := m.updateRunBranchPrompt(tea.KeyMsg{Type: tea.KeyEnter})
	hub := nm.(monitorModel)
	if got := hub.runTickets[0].branch; got != "magneton/local-9-fix" {
		t.Errorf("confirmed branch: got %q", got)
	}
	if hub.runBranchPrompt != -1 || hub.runImgPrompt != 0 {
		t.Errorf("Enter should advance to images: branchPrompt=%d imgPrompt=%d",
			hub.runBranchPrompt, hub.runImgPrompt)
	}
}

// The branch field supports in-place caret editing: ←→ move the cursor and
// typing inserts at it (shared formField editor), so the middle of the name
// can be fixed without deleting from the end.
func TestBranchPromptCursorEditing(t *testing.T) {
	m := monitorModel{
		view: viewRunInput, runMode: "content",
		runTickets:      []pendingTicket{{id: "LOCAL-9", kind: "content", title: "t", lines: 1, branch: "main"}},
		runIDPrompt:     -1,
		runBranchPrompt: 0, runImgPrompt: -1,
		promptCursor: len("main"),
	}
	step := func(msg tea.KeyMsg) {
		nm, _ := m.updateRunBranchPrompt(msg)
		m = nm.(monitorModel)
	}
	step(tea.KeyMsg{Type: tea.KeyLeft})
	step(tea.KeyMsg{Type: tea.KeyLeft})
	step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("XY")})
	if got := m.runTickets[0].branch; got != "maXYin" {
		t.Errorf("insert mid-value = %q, want %q", got, "maXYin")
	}
	step(tea.KeyMsg{Type: tea.KeyBackspace})
	if got := m.runTickets[0].branch; got != "maXin" {
		t.Errorf("backspace mid-value = %q, want %q", got, "maXin")
	}
	step(tea.KeyMsg{Type: tea.KeyHome})
	step(tea.KeyMsg{Type: tea.KeyDelete})
	if got := m.runTickets[0].branch; got != "aXin" {
		t.Errorf("delete at home = %q, want %q", got, "aXin")
	}
}

// Enter on an emptied branch field must not advance - the branch is required.
func TestBranchPromptEnterRejectsEmpty(t *testing.T) {
	m := monitorModel{
		view: viewRunInput, runMode: "content",
		runTickets:      []pendingTicket{{id: "LOCAL-9", kind: "content", title: "t", lines: 1, branch: "   "}},
		runBranchPrompt: 0, runImgPrompt: -1,
	}
	nm, _ := m.updateRunBranchPrompt(tea.KeyMsg{Type: tea.KeyEnter})
	hub := nm.(monitorModel)
	if hub.runBranchPrompt != 0 || hub.runImgPrompt != -1 {
		t.Errorf("empty branch should keep the prompt open: branchPrompt=%d imgPrompt=%d",
			hub.runBranchPrompt, hub.runImgPrompt)
	}
}

// The branch prompt screen: no chip row, the full ticket content rendered
// above, and the header sits directly above the branch field.
func TestBranchPromptRender(t *testing.T) {
	m := monitorModel{
		view: viewRunInput, runMode: "content", height: 30,
		runTickets: []pendingTicket{{
			id: "LOCAL-9", kind: "content", title: "t", lines: 2,
			body: "Title: fix the crash\n\nDetails here", branch: "magneton/local-9",
		}},
		runIDPrompt: -1, runBranchPrompt: 0, runImgPrompt: -1, runStackPrompt: -1, runReviewPrompt: -1,
		promptCursor: len("magneton/local-9"), // caret at the end, as when the prompt opens
	}
	// Strip ANSI styling: glamour splits words across escape-coded segments,
	// which would defeat plain substring checks.
	out := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(m.renderRunInput(80), "")
	if !strings.Contains(out, "Update or Approve the branch name") {
		t.Errorf("header missing:\n%s", out)
	}
	if !strings.Contains(out, "branch › magneton/local-9") {
		t.Errorf("branch field missing:\n%s", out)
	}
	if !strings.Contains(out, "Details here") {
		t.Errorf("full ticket content should render:\n%s", out)
	}
	if strings.Contains(out, "· 2 lines") {
		t.Errorf("chip row must not render on the branch prompt:\n%s", out)
	}
	// The header must sit below the content, directly above the branch field.
	if strings.Index(out, "Update or Approve") < strings.Index(out, "Details here") {
		t.Errorf("header should render below the ticket content:\n%s", out)
	}
}

// applyTicketEdit folds an editor rewrite back into the chip and re-infers the
// branch when the user hadn't customized it.
func TestApplyTicketEditReinfersBranch(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(&config.Config{
		Repos: []config.Repo{{Path: "/r", Branch: "magneton/{ticket}"}},
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "edited.md")
	if err := os.WriteFile(path, []byte("PROJ-77 New title\n\nnew body"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := monitorModel{
		runTickets: []pendingTicket{{
			id: "PROJ-1", kind: "content", title: "Old title", lines: 1,
			body: "PROJ-1 Old title", branch: inferBranch("PROJ-1", "Old title"),
		}},
		runBranchPrompt: 0,
	}
	nm, _ := m.applyTicketEdit(ticketEditedMsg{i: 0, path: path})
	hub := nm.(monitorModel)
	got := hub.runTickets[0]
	if got.id != "PROJ-77" || got.lines != 3 || !strings.Contains(got.body, "new body") {
		t.Errorf("edit not applied: %+v", got)
	}
	if got.branch != "magneton/proj-77" {
		t.Errorf("uncustomized branch should re-infer, got %q", got.branch)
	}
}

// applyTicketEdit must NOT overwrite a branch the user already customized.
func TestApplyTicketEditKeepsCustomBranch(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "edited.md")
	if err := os.WriteFile(path, []byte("PROJ-77 New title"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := monitorModel{
		runTickets: []pendingTicket{{
			id: "PROJ-1", kind: "content", title: "Old title", lines: 1,
			body: "PROJ-1 Old title", branch: "my/custom-name",
		}},
		runBranchPrompt: 0,
	}
	nm, _ := m.applyTicketEdit(ticketEditedMsg{i: 0, path: path})
	if got := nm.(monitorModel).runTickets[0].branch; got != "my/custom-name" {
		t.Errorf("customized branch must survive an edit, got %q", got)
	}
}

// An edit that empties the ticket is discarded (the chip keeps its old body).
func TestApplyTicketEditRejectsEmpty(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "edited.md")
	if err := os.WriteFile(path, []byte("   \n  "), 0o644); err != nil {
		t.Fatal(err)
	}
	m := monitorModel{
		runTickets:      []pendingTicket{{id: "PROJ-1", kind: "content", body: "keep me", branch: "b"}},
		runBranchPrompt: 0,
	}
	nm, _ := m.applyTicketEdit(ticketEditedMsg{i: 0, path: path})
	hub := nm.(monitorModel)
	if hub.runTickets[0].body != "keep me" {
		t.Errorf("empty edit should be discarded, got body %q", hub.runTickets[0].body)
	}
	if hub.notice == "" {
		t.Error("discarding an empty edit should set a notice")
	}
}

// Esc on the branch prompt drops the chip entirely.
func TestBranchPromptEscDropsChip(t *testing.T) {
	m := monitorModel{
		view: viewRunInput, runMode: "content",
		runTickets:      []pendingTicket{{id: "LOCAL-9", kind: "content", title: "t", lines: 1, branch: "b"}},
		runBranchPrompt: 0, runImgPrompt: -1,
	}
	nm, _ := m.updateRunBranchPrompt(tea.KeyMsg{Type: tea.KeyEsc})
	hub := nm.(monitorModel)
	if len(hub.runTickets) != 0 || hub.runBranchPrompt != -1 {
		t.Errorf("Esc should drop the chip: tickets=%+v branchPrompt=%d",
			hub.runTickets, hub.runBranchPrompt)
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
