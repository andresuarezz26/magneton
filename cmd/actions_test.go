package cmd

import (
	"testing"

	"github.com/andresuarezz26/magneton/internal/store"
)

func actionIDs(btns []actionBtn) map[string]bool {
	m := map[string]bool{}
	for _, b := range btns {
		m[b.id] = true
	}
	return m
}

func TestCurrentActionsContextual(t *testing.T) {
	// awaiting-answer → Answer is the primary CTA.
	m := monitorModel{flat: []store.Session{{Ticket: "K1", State: "awaiting-answer"}}}
	ids := actionIDs(m.currentActions())
	for _, want := range []string{"answer", "open", "stop", "run", "menu", "quit"} {
		if !ids[want] {
			t.Errorf("awaiting: missing action %q", want)
		}
	}
	if ids["resume"] {
		t.Error("awaiting should not offer resume")
	}

	// failed → Resume is offered.
	m = monitorModel{flat: []store.Session{{Ticket: "K1", State: "failed"}}}
	if !actionIDs(m.currentActions())["resume"] {
		t.Error("failed should offer resume")
	}

	// review (terminal) → no Stop.
	m = monitorModel{flat: []store.Session{{Ticket: "K1", State: "review"}}}
	if actionIDs(m.currentActions())["stop"] {
		t.Error("review (terminal) should not offer stop")
	}

	// confirming → Yes/No.
	m = monitorModel{confirming: "K1"}
	ids = actionIDs(m.currentActions())
	if !ids["confirm-yes"] || !ids["confirm-no"] {
		t.Errorf("confirming should offer yes/no, got %v", ids)
	}
}

func TestDoActionTransitions(t *testing.T) {
	if mm, _ := (monitorModel{}).doAction("run"); mm.(monitorModel).view != viewRunInput {
		t.Error("run → run-input view")
	}
	if mm, _ := (monitorModel{}).doAction("menu"); mm.(monitorModel).view != viewPalette {
		t.Error("menu → palette view")
	}
	m := monitorModel{flat: []store.Session{{Ticket: "K1", State: "failed"}}}
	if mm, _ := m.doAction("stop"); mm.(monitorModel).confirming != "K1" {
		t.Error("stop → confirming set to the selected ticket")
	}
}

func TestRenderActionBarHitboxesOrdered(t *testing.T) {
	m := monitorModel{flat: []store.Session{{Ticket: "K1", State: "awaiting-answer"}}}
	_, boxes := m.renderActionBar()
	if len(boxes) < 3 {
		t.Fatalf("expected several buttons, got %d", len(boxes))
	}
	for i := 1; i < len(boxes); i++ {
		if boxes[i].x0 <= boxes[i-1].x1 {
			t.Errorf("hitboxes overlap or out of order: %+v then %+v", boxes[i-1], boxes[i])
		}
	}
}
