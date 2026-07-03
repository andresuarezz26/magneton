package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/andresuarezz26/magneton/internal/paths"
)

// ticketSpec is one resolved unit of work, from Jira or a local file.
// It is source-agnostic: the runner only ever sees ticket/summary/desc.
type ticketSpec struct {
	ticket     string // CLEAN id, safe for WorktreeFor/branch/LogFor
	summary    string // may be "" for Jira (filled in after FetchIssue)
	desc       string
	local      bool     // true => skip Jira fetch/transition and never comment to Jira
	sourcePath string   // absolute path to the .md file; empty for Jira tickets
	images     []string // image files attached to the ticket (pasted-content flow)
}

var (
	h1Re    = regexp.MustCompile(`^#\s+(.*\S)\s*$`)
	nonIDRe = regexp.MustCompile(`[^A-Z0-9]+`)
	// ticketIDRe matches a Jira-style key anywhere in text: a project key (a
	// letter followed by letters/digits) then a dash and a number, e.g. PROJ-123.
	ticketIDRe = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9]*-\d+)\b`)
	// ticketKeyRe is ticketIDRe anchored: the whole (trimmed, single-line) string
	// IS a bare key. Used to classify a pasted token as a Jira key vs content.
	ticketKeyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-\d+$`)
)

// sectionHeads are generic section headings that are not real ticket titles.
// Spec-/template-style tickets often open with "# Description" or
// "# Acceptance Criteria"; using those as the title is useless, so we skip them
// and fall back to the first real line of prose.
var sectionHeads = map[string]bool{
	"description":          true,
	"summary":              true,
	"overview":             true,
	"context":              true,
	"background":           true,
	"details":              true,
	"notes":                true,
	"dependencies":         true,
	"acceptance criteria":  true,
	"requirements":         true,
	"implementation":       true,
	"implementation notes": true,
	"goal":                 true,
	"goals":                true,
	"non-goals":            true,
	"problem":              true,
	"solution":             true,
	"tasks":                true,
	"scope":                true,
	"testing":              true,
	"test plan":            true,
}

// maxDerivedTitleLen caps a title derived from a body line so a long opening
// sentence doesn't become an unwieldy PR title. H1 titles are left untouched.
const maxDerivedTitleLen = 100

// loadLocalTicket reads a markdown/text file into a ticketSpec.
// Title = first H1 ("# ...") line, else the first non-blank line.
// Body  = everything after the H1 line (or the full content if there is no H1).
// The ticket id is derived from the base filename (see ticketIDFromPath).
func loadLocalTicket(path string) (ticketSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ticketSpec{}, fmt.Errorf("read %s: %w", path, err)
	}
	title, body, err := parseTicketContent(string(raw))
	if err != nil {
		return ticketSpec{}, fmt.Errorf("%s: %w", path, err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	id := ticketIDFromPath(path)
	return ticketSpec{
		ticket:     id,
		summary:    stripIDPrefix(title, id),
		desc:       body,
		local:      true,
		sourcePath: abs,
		images:     discoverPastedImages(abs),
	}, nil
}

// discoverPastedImages returns sibling image files when the ticket .md lives in a
// magneton-controlled pasted dir (~/.agent/pasted/<id>/). Returns nil for a user's
// own .md file, so we never sweep in unrelated images from their repo.
func discoverPastedImages(mdPath string) []string {
	dir := filepath.Dir(mdPath)
	pastedAbs, err := filepath.Abs(paths.Pasted())
	if err != nil || !strings.HasPrefix(dir, pastedAbs+string(os.PathSeparator)) {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var imgs []string
	for _, e := range entries {
		if !e.IsDir() && isImageExt(e.Name()) {
			imgs = append(imgs, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(imgs)
	return imgs
}

// isImageExt reports whether name has a supported image extension.
func isImageExt(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

// stripIDPrefix removes a leading ticket-id from a title so it isn't shown (and
// slugified into the branch) twice. Jira-style titles often read "TICKET-3 — Add
// X"; with the id already in the chip and the "ai/{ticket}-{slug}" branch, that
// prefix is redundant. Only strips when a separator (or end) follows the id, so
// "TICKET-30 …" is left intact when the id is "TICKET-3".
func stripIDPrefix(title, id string) string {
	if id == "" {
		return title
	}
	t := strings.TrimSpace(title)
	if len(t) < len(id) || !strings.EqualFold(t[:len(id)], id) {
		return title
	}
	rest := t[len(id):]
	if rest == "" {
		return title // title is only the id
	}
	trimmed := strings.TrimLeft(rest, " \t-—–:·|.")
	if trimmed == rest || trimmed == "" {
		return title // no separator after the id, or nothing left
	}
	return trimmed
}

// parseTicketContent derives a (title, body) from raw markdown/text ticket
// content. Title = first H1 ("# ...") that is a real title (not a generic
// section header like "# Description"), else the first non-blank line of prose.
// Body = everything after that H1, or the whole content when there is no H1.
// Shared by loadLocalTicket (file source) and pasted-content tickets (TUI).
func parseTicketContent(raw string) (title, body string, err error) {
	lines := strings.Split(raw, "\n")
	body = raw
	for i, line := range lines {
		m := h1Re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if sectionHeads[strings.ToLower(m[1])] {
			continue // a section header, not a title — keep the body intact
		}
		title = m[1]
		body = strings.TrimLeft(strings.Join(lines[i+1:], "\n"), "\n")
		break
	}
	if title == "" {
		for _, line := range lines {
			s := strings.TrimSpace(line)
			if s == "" || strings.HasPrefix(s, "#") {
				continue
			}
			title = truncateTitle(s)
			break
		}
	}
	if title == "" {
		return "", "", fmt.Errorf("could not derive a title (empty content?)")
	}
	return title, strings.TrimSpace(body), nil
}

// detectTicketID pulls the first Jira-style key (e.g. PROJ-123) out of free-form
// ticket text — the instant, offline fast-path before falling back to an AI
// extraction pass. Returns the id uppercased, or ("", false) when none is found.
func detectTicketID(content string) (string, bool) {
	if m := ticketIDRe.FindStringSubmatch(content); m != nil {
		return strings.ToUpper(m[1]), true
	}
	return "", false
}

// normalizeNewlines converts CRLF and lone CR line endings to LF. Terminal
// bracketed paste transmits newlines as carriage returns, so pasted content
// arrives with \r; without this, line counting and title parsing see one giant
// line and stray \r carriage-returns corrupt the on-screen render.
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// isTicketKey reports whether s (trimmed, single line) is itself a bare Jira key.
// Used to classify a pasted token as a Jira key rather than ticket content.
func isTicketKey(s string) bool {
	return ticketKeyRe.MatchString(strings.TrimSpace(s))
}

// truncateTitle shortens an over-long body-derived title at a word boundary,
// appending an ellipsis. Short titles pass through unchanged.
func truncateTitle(s string) string {
	r := []rune(s)
	if len(r) <= maxDerivedTitleLen {
		return s
	}
	cut := string(r[:maxDerivedTitleLen])
	if i := strings.LastIndex(cut, " "); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "…"
}

// ticketIDFromPath turns a/b/ticket_1.md into "TICKET-1": strip dir + extension,
// uppercase, collapse every run of non-[A-Z0-9] into a single '-', trim '-'.
// Falls back to "LOCAL" if nothing usable remains. The result is safe to feed
// into paths.WorktreeFor / paths.LogFor and the "ai/{ticket}-{slug}" template.
func ticketIDFromPath(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	id := nonIDRe.ReplaceAllString(strings.ToUpper(base), "-")
	id = strings.Trim(id, "-")
	if id == "" {
		return "LOCAL"
	}
	return id
}
