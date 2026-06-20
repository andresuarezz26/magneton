package cmd

import (
	"testing"

	"github.com/andresuarezz26/magneton/internal/store"
)

func itemIDs(items []paletteItem) map[string]bool {
	m := map[string]bool{}
	for _, it := range items {
		m[it.key] = true
	}
	return m
}

func TestAgentActionsContextual(t *testing.T) {
	// awaiting-answer → Answer offered, not Resume.
	ids := itemIDs(agentActions(store.Session{Ticket: "K1", State: "awaiting-answer"}))
	for _, want := range []string{"answer", "studio", "claude", "stop"} {
		if !ids[want] {
			t.Errorf("awaiting: missing action %q", want)
		}
	}
	if ids["resume"] {
		t.Error("awaiting should not offer resume")
	}

	// failed → Resume offered.
	if !itemIDs(agentActions(store.Session{State: "failed"}))["resume"] {
		t.Error("failed should offer resume")
	}

	// review (terminal) → no Stop.
	if itemIDs(agentActions(store.Session{State: "review"}))["stop"] {
		t.Error("review (terminal) should not offer stop")
	}
}

func TestPaletteItemsIncludeGlobals(t *testing.T) {
	t.Setenv("MAGNETON_HOME", t.TempDir()) // no pidfile → daemon stopped
	m := monitorModel{flat: []store.Session{{Ticket: "K1", State: "awaiting-answer"}}}
	ids := itemIDs(m.paletteItems())
	for _, want := range []string{"answer", "run", "doctor", "config", "setup", "daemon-start", "quit"} {
		if !ids[want] {
			t.Errorf("menu missing %q", want)
		}
	}
	if ids["daemon-stop"] {
		t.Error("daemon stopped should not offer daemon-stop")
	}
}

func TestDoActionTransitions(t *testing.T) {
	if mm, _ := (monitorModel{}).doAction("menu"); mm.(monitorModel).view != viewPalette {
		t.Error("menu → palette view")
	}
	if mm, _ := (monitorModel{}).doAction("run"); mm.(monitorModel).view != viewRunInput {
		t.Error("run → run-input view")
	}
	m := monitorModel{flat: []store.Session{{Ticket: "K1", State: "failed"}}}
	if mm, _ := m.doAction("stop"); mm.(monitorModel).confirming != "K1" {
		t.Error("stop → confirming set to the selected ticket")
	}
}
