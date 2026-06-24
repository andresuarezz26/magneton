package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ticketSpec is one resolved unit of work, from Jira or a local file.
// It is source-agnostic: the runner only ever sees ticket/summary/desc.
type ticketSpec struct {
	ticket     string // CLEAN id, safe for WorktreeFor/branch/LogFor
	summary    string // may be "" for Jira (filled in after FetchIssue)
	desc       string
	local      bool   // true => skip Jira fetch/transition and never comment to Jira
	sourcePath string // absolute path to the .md file; empty for Jira tickets
}

var (
	h1Re    = regexp.MustCompile(`^#\s+(.*\S)\s*$`)
	nonIDRe = regexp.MustCompile(`[^A-Z0-9]+`)
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
	lines := strings.Split(string(raw), "\n")

	// Title = first H1 that is a real title (not a generic section header like
	// "# Description"). Body = everything after that H1.
	title, body := "", string(raw)
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
	// No usable H1 → first line of real prose (skip blank and heading lines so
	// "# Description" itself is never chosen).
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
		return ticketSpec{}, fmt.Errorf("%s: could not derive a title (file is empty?)", path)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return ticketSpec{
		ticket:     ticketIDFromPath(path),
		summary:    title,
		desc:       strings.TrimSpace(body),
		local:      true,
		sourcePath: abs,
	}, nil
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
