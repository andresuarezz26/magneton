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

	title, body := "", string(raw)
	for i, line := range lines {
		if m := h1Re.FindStringSubmatch(line); m != nil {
			title = m[1]
			body = strings.TrimLeft(strings.Join(lines[i+1:], "\n"), "\n")
			break
		}
	}
	if title == "" {
		for _, line := range lines {
			if s := strings.TrimSpace(line); s != "" {
				title = s
				break
			}
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
