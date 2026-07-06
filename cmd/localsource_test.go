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

func TestParseTicketContent(t *testing.T) {
	// H1 title → title + body after it.
	title, body, err := parseTicketContent("# Add a file\n\nCreate one.txt.\n")
	if err != nil || title != "Add a file" || body != "Create one.txt." {
		t.Errorf("h1: title=%q body=%q err=%v", title, body, err)
	}

	// Generic section header is skipped; title comes from first prose line and
	// the "# Description" heading stays in the body.
	title, body, err = parseTicketContent("# Description\n\nUpdate the nav.\n")
	if err != nil || title != "Update the nav." {
		t.Errorf("section: title=%q err=%v", title, err)
	}
	if !strings.HasPrefix(body, "# Description") {
		t.Errorf("section: body should retain heading, got %q", body)
	}

	// No H1 → first non-blank line is the title.
	title, _, err = parseTicketContent("\nDo the thing\nmore\n")
	if err != nil || title != "Do the thing" {
		t.Errorf("noH1: title=%q err=%v", title, err)
	}

	// Empty content → error.
	if _, _, err := parseTicketContent("\n  \n"); err == nil {
		t.Error("empty: expected an error")
	}
}

func TestDetectTicketID(t *testing.T) {
	found := map[string]string{
		"Please fix PROJ-123 today":                 "PROJ-123",
		"[ABC-45] Add pull to refresh":              "ABC-45",
		"see https://x.atlassian.net/browse/PLEX-7": "PLEX-7",
		"lower proj-9 works":                        "PROJ-9",
		"# TICKET-4 - Empty State for Saved Papers": "TICKET-4",
	}
	for in, want := range found {
		if got, ok := detectTicketID(in); !ok || got != want {
			t.Errorf("detectTicketID(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"Add pull to refresh", "no id here 123", "release v2 build"} {
		if got, ok := detectTicketID(in); ok {
			t.Errorf("detectTicketID(%q) unexpectedly matched %q", in, got)
		}
	}
}

func TestIsTicketKey(t *testing.T) {
	yes := []string{"PROJ-123", "proj-1", "  ABC-45  ", "A1-9"}
	no := []string{"Add pull to refresh", "PROJ123", "PROJ-", "-1", "", "line1\nPROJ-1"}
	for _, s := range yes {
		if !isTicketKey(s) {
			t.Errorf("isTicketKey(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isTicketKey(s) {
			t.Errorf("isTicketKey(%q) = true, want false", s)
		}
	}
}

func TestStripIDPrefix(t *testing.T) {
	cases := []struct{ title, id, want string }{
		{"TICKET-3 - Add Pull-to-Refresh on Feed", "TICKET-3", "Add Pull-to-Refresh on Feed"},
		{"PROJ-1: Do the thing", "PROJ-1", "Do the thing"},
		{"PROJ-1 - fix bug", "PROJ-1", "fix bug"},
		{"ticket-3 - lower key", "TICKET-3", "lower key"},  // case-insensitive
		{"Add a file", "TICKET-1", "Add a file"},           // no prefix → unchanged
		{"TICKET-30 stuff", "TICKET-3", "TICKET-30 stuff"}, // no separator after id → unchanged
		{"TICKET-3", "TICKET-3", "TICKET-3"},               // title is only the id
		{"anything", "", "anything"},                       // no id
	}
	for _, c := range cases {
		if got := stripIDPrefix(c.title, c.id); got != c.want {
			t.Errorf("stripIDPrefix(%q, %q) = %q, want %q", c.title, c.id, got, c.want)
		}
	}
}

func TestParseDroppedPaths(t *testing.T) {
	cases := map[string][]string{
		"/tmp/a.png":                       {"/tmp/a.png"},
		"/tmp/a.png /tmp/b.jpg":            {"/tmp/a.png", "/tmp/b.jpg"},
		`/My\ Files/x.png`:                 {"/My Files/x.png"}, // backslash-escaped space
		`'/My Files/y.png'`:                {"/My Files/y.png"}, // single-quoted
		`"/a b/z.png"`:                     {"/a b/z.png"},      // double-quoted
		"/tmp/a.png\n":                     {"/tmp/a.png"},      // trailing newline
		`'/one two.png' "/three four.png"`: {"/one two.png", "/three four.png"},
	}
	for in, want := range cases {
		got := parseDroppedPaths(in)
		if len(got) != len(want) {
			t.Errorf("parseDroppedPaths(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parseDroppedPaths(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}

func TestIsImageExt(t *testing.T) {
	yes := []string{"a.png", "b.PNG", "c.jpg", "d.jpeg", "e.gif", "f.webp", "/x/y.Png"}
	no := []string{"a.txt", "b.md", "c", "d.pngx", "notes.png.md"}
	for _, s := range yes {
		if !isImageExt(s) {
			t.Errorf("isImageExt(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isImageExt(s) {
			t.Errorf("isImageExt(%q) = true, want false", s)
		}
	}
}

func TestNormalizeNewlines(t *testing.T) {
	if got := normalizeNewlines("a\r\nb\rc\nd"); got != "a\nb\nc\nd" {
		t.Errorf("normalizeNewlines = %q", got)
	}
}

// TestPastedContentCRLF reproduces the real bug: a terminal paste delivers
// newlines as carriage returns, so without normalization the whole ticket looks
// like one line and the title/render get corrupted.
func TestPastedContentCRLF(t *testing.T) {
	// The ticket the user pasted, with \r line endings as a terminal sends them.
	raw := "# TICKET-3 - Add Pull-to-Refresh on Feed\r\r## Summary\rAdd pull-to-refresh.\r\r## Priority\rLow\r"
	blob := normalizeNewlines(raw)

	if n := lineCount(blob); n != 7 {
		t.Errorf("lineCount = %d, want 7 (paste was seen as 1 line before the fix)", n)
	}
	title, _, err := parseTicketContent(blob)
	if err != nil || title != "TICKET-3 - Add Pull-to-Refresh on Feed" {
		t.Errorf("title = %q, err = %v", title, err)
	}
	if id, ok := detectTicketID(blob); !ok || id != "TICKET-3" {
		t.Errorf("detectTicketID = %q, %v; want TICKET-3", id, ok)
	}
	if strings.ContainsRune(title, '\r') {
		t.Error("title still contains a carriage return")
	}
}

func TestLineCount(t *testing.T) {
	cases := map[string]int{"": 0, "a": 1, "a\n": 1, "a\nb": 2, "a\nb\n": 2, "\n\n": 0}
	for in, want := range cases {
		if got := lineCount(in); got != want {
			t.Errorf("lineCount(%q) = %d, want %d", in, got, want)
		}
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
