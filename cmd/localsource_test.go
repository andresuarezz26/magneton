package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTicketIDFromPath(t *testing.T) {
	cases := map[string]string{
		"a/b/ticket_1.md":   "TICKET-1",
		"ticket_1.md":       "TICKET-1",
		"fix bug.md":        "FIX-BUG",
		"feature.spec.md":   "FEATURE-SPEC",
		"/tmp/Add-File.txt": "ADD-FILE",
		"___.md":            "LOCAL",
		"UPPER.md":          "UPPER",
	}
	for in, want := range cases {
		if got := ticketIDFromPath(in); got != want {
			t.Errorf("ticketIDFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadLocalTicket(t *testing.T) {
	dir := t.TempDir()

	h1 := filepath.Join(dir, "ticket_1.md")
	if err := os.WriteFile(h1, []byte("# Add a file\n\nCreate one.txt with content.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sp, err := loadLocalTicket(h1)
	if err != nil {
		t.Fatal(err)
	}
	if sp.ticket != "TICKET-1" || sp.summary != "Add a file" {
		t.Errorf("h1: got ticket=%q summary=%q", sp.ticket, sp.summary)
	}
	if sp.desc != "Create one.txt with content." {
		t.Errorf("h1: body = %q", sp.desc)
	}
	if !sp.local {
		t.Error("h1: expected local=true")
	}

	// No H1 → first non-blank line is the title, full content is the body.
	noH1 := filepath.Join(dir, "plain.md")
	if err := os.WriteFile(noH1, []byte("\nDo the thing\nmore detail\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sp, err = loadLocalTicket(noH1)
	if err != nil {
		t.Fatal(err)
	}
	if sp.summary != "Do the thing" {
		t.Errorf("noH1: summary = %q", sp.summary)
	}

	// First H1 is a generic section header → skip it; title comes from the first
	// real prose line, not "Description".
	section := filepath.Join(dir, "PLEX-1.md")
	if err := os.WriteFile(section, []byte("# Description\n\nUpdate tour_nav.xml to add destinations.\n\n# Acceptance Criteria\n\n- updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sp, err = loadLocalTicket(section)
	if err != nil {
		t.Fatal(err)
	}
	if sp.summary != "Update tour_nav.xml to add destinations." {
		t.Errorf("section: summary = %q", sp.summary)
	}
	// Body keeps its structure (the "# Description" heading is not consumed).
	if !strings.HasPrefix(sp.desc, "# Description") {
		t.Errorf("section: body should retain structure, got %q", sp.desc)
	}

	// Long opening sentence is truncated to a sane title length.
	long := filepath.Join(dir, "PLEX-2.md")
	bigLine := "Update Compass/compass/src/main/res/navigation/tour_nav.xml to add all new tours fragment destinations and change the start destination"
	if err := os.WriteFile(long, []byte("# Description\n\n"+bigLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sp, err = loadLocalTicket(long)
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(sp.summary)) > maxDerivedTitleLen+1 || !strings.HasSuffix(sp.summary, "…") {
		t.Errorf("long: summary not truncated: %q (len %d)", sp.summary, len([]rune(sp.summary)))
	}

	// Empty file → error.
	empty := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(empty, []byte("\n  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLocalTicket(empty); err == nil {
		t.Error("empty file: expected an error, got nil")
	}
}

func TestDedupeSpecs(t *testing.T) {
	in := []ticketSpec{
		{ticket: "DUP"}, {ticket: "DUP"}, {ticket: "DUP"}, {ticket: "OTHER"},
	}
	out := dedupeSpecs(in)
	got := []string{out[0].ticket, out[1].ticket, out[2].ticket, out[3].ticket}
	want := []string{"DUP", "DUP-2", "DUP-3", "OTHER"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dedupe[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveSpecs(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "ticket_9.md")
	if err := os.WriteFile(f, []byte("# Nine\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Reset package-level flag state between sub-cases.
	reset := func() { runLocal, runTitle, runDesc = false, "", "" }

	t.Run("jira key fallback", func(t *testing.T) {
		reset()
		specs, err := resolveSpecs([]string{"proj-123"})
		if err != nil {
			t.Fatal(err)
		}
		if len(specs) != 1 || specs[0].ticket != "PROJ-123" || specs[0].local {
			t.Errorf("got %+v", specs)
		}
	})

	t.Run("file is local", func(t *testing.T) {
		reset()
		specs, err := resolveSpecs([]string{f})
		if err != nil {
			t.Fatal(err)
		}
		if len(specs) != 1 || specs[0].ticket != "TICKET-9" || !specs[0].local || specs[0].summary != "Nine" {
			t.Errorf("got %+v", specs)
		}
	})

	t.Run("mixed file + key", func(t *testing.T) {
		reset()
		specs, err := resolveSpecs([]string{f, "ABC-1"})
		if err != nil {
			t.Fatal(err)
		}
		if len(specs) != 2 || !specs[0].local || specs[1].local {
			t.Errorf("got %+v", specs)
		}
	})

	t.Run("legacy --local needs --title", func(t *testing.T) {
		reset()
		runLocal = true
		if _, err := resolveSpecs([]string{"HELLO-1"}); err == nil {
			t.Error("expected error for --local without --title")
		}
	})

	t.Run("legacy single --title", func(t *testing.T) {
		reset()
		runLocal, runTitle = true, "do x"
		specs, err := resolveSpecs([]string{"HELLO-1"})
		if err != nil {
			t.Fatal(err)
		}
		if len(specs) != 1 || specs[0].ticket != "HELLO-1" || specs[0].summary != "do x" || !specs[0].local {
			t.Errorf("got %+v", specs)
		}
	})

	t.Run("--title rejected with multiple args", func(t *testing.T) {
		reset()
		runTitle = "x"
		if _, err := resolveSpecs([]string{f, "ABC-1"}); err == nil {
			t.Error("expected error: --title with multiple args")
		}
	})
}
