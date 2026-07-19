package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/git"
	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/secrets"
)

// hubView is the top-level view of the hub. The zero value is the dashboard.
type hubView int

const (
	viewDashboard hubView = iota
	viewPalette
	viewRunMethod
	viewRunInput
	viewOutput
	viewForm
	viewConsent
	viewPlan // full-screen markdown plan viewer (plan-review gate)
)

// ---- messages --------------------------------------------------------------

type doctorDoneMsg struct{ out string }
type formDoneMsg struct {
	notice string
	err    error
}
type runLaunchedMsg struct {
	text string
	err  error
}

// pendingTicket is one queued ticket in the "Start new ticket(s)" input, shown
// as a chip. kind is content|jira.
type pendingTicket struct {
	id         string // ticket id (typed/confirmed; also the edit buffer while prompting)
	title      string // parsed title (or the key itself, for a jira chip)
	lines      int
	kind       string   // "content" | "jira"
	body       string   // raw pasted content (content kind)
	images     []string // attached image files (content kind)
	base       string   // chosen base branch name (bare); "" = default
	reviewPlan bool     // pause after the plan stage for human approval
}
type daemonMsg struct {
	action string // "start" | "stop"
	err    error
}

// ---- command palette -------------------------------------------------------

type paletteItem struct {
	key   string
	label string
	desc  string
}

// paletteItems is the palette menu. Opened on a specific agent (Enter on its
// row) it shows ONLY that agent's actions; opened as the command menu (":") it
// shows the global commands too. Rebuilt each open so it reflects the current
// selection/daemon.
func (m monitorModel) paletteItems() []paletteItem {
	var items []paletteItem
	if s := m.selected(); s != nil {
		items = append(items, agentActions(*s)...)
	}
	if m.paletteAgentOnly {
		return items // scoped to the selected agent's actions
	}
	items = append(items,
		paletteItem{"run", "Start new ticket(s)", "launch a ticket key or .md file"},
		paletteItem{"doctor", "Doctor", "connectivity + setup health check"},
		paletteItem{"config", "Edit config", "edit ~/.magneton/config.toml fields"},
		paletteItem{"setup", "Setup wizard", "configure repo, models, and token"},
	)
	if _, ok := daemonAlive(); ok {
		items = append(items, paletteItem{"daemon-stop", "Stop daemon", "stop the background poller"})
	} else {
		items = append(items, paletteItem{"daemon-start", "Start daemon", "poll Jira and run the fleet in the background"})
	}
	return append(items, paletteItem{"quit", "Quit", "exit the hub"})
}

func (m monitorModel) updatePalette(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	items := m.paletteItems()
	switch msg.String() {
	case "esc", "q":
		m.view = viewDashboard
	case "up", "k":
		if m.paletteCursor > 0 {
			m.paletteCursor--
		}
	case "down", "j":
		if m.paletteCursor < len(items)-1 {
			m.paletteCursor++
		}
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		if m.paletteCursor < len(items) {
			id := items[m.paletteCursor].key
			m.view = viewDashboard // close the menu; doAction may reopen another view
			return m.doAction(id)
		}
	}
	return m, nil
}

func (m monitorModel) renderPalette(w int) string {
	var b strings.Builder
	title := "Actions"
	if s := m.selected(); s != nil {
		title = "Actions - " + s.Ticket
	}
	b.WriteString(headerStyle.Render("  "+title) + "\n\n")
	for i, it := range m.paletteItems() {
		marker := "   "
		label := it.label
		if i == m.paletteCursor {
			marker = " ▸ "
			label = selStyle.Render(" " + it.label + " ")
		}
		b.WriteString(truncate(marker+label+"  "+dimStyle.Render(it.desc), w) + "\n")
	}
	b.WriteString("\n  " + dimStyle.Render("↑↓ select · enter: choose · esc: close"))
	return b.String()
}

// ---- run-new: method picker -----------------------------------------------

type runMethod struct{ mode, label, desc string }

// runMethods lists the ways to add tickets. Jira appears only when configured.
func (m monitorModel) runMethods() []runMethod {
	ms := []runMethod{{"content", "Paste ticket content", "paste the text, confirm its id, attach images"}}
	if m.jira != nil {
		ms = append(ms, runMethod{"jira", "From Jira", "enter Jira ticket key(s)"})
	}
	return ms
}

func (m monitorModel) updateRunMethod(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ms := m.runMethods()
	switch msg.String() {
	case "esc", "q":
		m.view = viewDashboard
	case "up", "k":
		if m.runMethodCursor > 0 {
			m.runMethodCursor--
		}
	case "down", "j":
		if m.runMethodCursor < len(ms)-1 {
			m.runMethodCursor++
		}
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		if m.runMethodCursor < len(ms) {
			m.runMode = ms[m.runMethodCursor].mode
			m.runText, m.runTickets, m.runIDPrompt, m.runImgPrompt = "", nil, -1, -1
			m.view = viewRunInput
		}
	}
	return m, nil
}

func (m monitorModel) renderRunMethod(w int) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("  Start new ticket(s)") + "\n")
	b.WriteString(dimStyle.Render("  how do you want to add tickets?") + "\n\n")
	for i, it := range m.runMethods() {
		marker, label := "   ", it.label
		if i == m.runMethodCursor {
			marker, label = " ▸ ", selStyle.Render(" "+it.label+" ")
		}
		b.WriteString(truncate(marker+label+"  "+dimStyle.Render(it.desc), w) + "\n")
	}
	b.WriteString("\n  " + dimStyle.Render("↑↓ select · enter: choose · esc: cancel"))
	return b.String()
}

// ---- run-new: input (per method) ------------------------------------------

func (m monitorModel) updateRunInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.runIDPrompt >= 0 {
		return m.updateRunIDPrompt(msg)
	}
	if m.runImgPrompt >= 0 {
		return m.updateRunImgAttach(msg)
	}
	if m.runStackPrompt >= 0 {
		return m.updateRunStack(msg)
	}
	if m.runReviewPrompt >= 0 {
		return m.updateRunReview(msg)
	}
	switch m.runMode {
	case "content":
		return m.updateRunContent(msg)
	case "jira":
		return m.updateRunJira(msg)
	}
	m.view = viewRunMethod // no mode set → back to the picker
	return m, nil
}

func (m monitorModel) cancelRunInput() monitorModel {
	m.view = viewDashboard
	m.runMode, m.runText, m.runTickets = "", "", nil
	m.runIDPrompt, m.runImgPrompt, m.runStackPrompt = -1, -1, -1
	m.runReviewPrompt, m.reviewCursor = -1, 0
	m.stackBranches, m.stackDefault, m.stackFilter, m.stackCursor = nil, "", "", 0
	return m
}

func (m monitorModel) launchOrClose() (tea.Model, tea.Cmd) {
	if len(m.runTickets) == 0 {
		return m.cancelRunInput(), nil
	}
	tickets := m.runTickets
	m = m.cancelRunInput()
	return m, m.launchRun(tickets)
}

// content method: each paste is one ticket → confirm its id → attach images.
func (m monitorModel) updateRunContent(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Paste {
		return m.addContentTicket(string(msg.Runes))
	}
	switch msg.Type {
	case tea.KeyEnter:
		return m.launchOrClose()
	case tea.KeyEsc:
		return m.cancelRunInput(), nil
	case tea.KeyBackspace:
		if n := len(m.runTickets); n > 0 {
			m.runTickets = m.runTickets[:n-1]
		}
	default:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m monitorModel) addContentTicket(blob string) (tea.Model, tea.Cmd) {
	blob = normalizeNewlines(blob) // terminals paste newlines as \r
	if strings.TrimSpace(blob) == "" {
		return m, nil
	}
	title, _, err := parseTicketContent(blob)
	if err != nil || title == "" {
		title = "(untitled)"
	}
	guess, _ := detectTicketID(blob) // pre-fill only; the user always confirms
	m.runTickets = append(m.runTickets, pendingTicket{
		id: guess, title: title, lines: lineCount(blob), kind: "content", body: blob,
	})
	m.runIDPrompt = len(m.runTickets) - 1
	return m, nil
}

// jira method: whitespace-separated Jira keys become chips directly.
func (m monitorModel) updateRunJira(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Paste {
		return m.commitJira(string(msg.Runes)), nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		if strings.TrimSpace(m.runText) != "" {
			m = m.commitJira(m.runText)
			m.runText = ""
		}
		return m.launchOrClose()
	case tea.KeyEsc:
		return m.cancelRunInput(), nil
	case tea.KeyBackspace:
		if r := []rune(m.runText); len(r) > 0 {
			m.runText = string(r[:len(r)-1])
		} else if n := len(m.runTickets); n > 0 {
			m.runTickets = m.runTickets[:n-1]
		}
	case tea.KeySpace:
		if strings.TrimSpace(m.runText) != "" {
			m = m.commitJira(m.runText)
			m.runText = ""
		}
	case tea.KeyRunes:
		m.runText += string(msg.Runes)
	default:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "ctrl+s":
			// Open the stack picker for the last chip, if any.
			if n := len(m.runTickets); n > 0 {
				return m.openStackPicker(n - 1)
			}
		}
	}
	return m, nil
}

// commitJira turns whitespace-separated Jira keys into chips.
func (m monitorModel) commitJira(s string) monitorModel {
	for _, tok := range strings.Fields(s) {
		m.runTickets = append(m.runTickets, pendingTicket{
			id: normalizeTicket(tok), title: tok, kind: "jira",
		})
	}
	return m
}

// updateRunImgAttach: drag image files into the terminal (their paths arrive as
// text) to attach them to the content ticket. Enter on an empty line finishes.
func (m monitorModel) updateRunImgAttach(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	i := m.runImgPrompt
	if i < 0 || i >= len(m.runTickets) {
		m.runImgPrompt, m.runText = -1, ""
		return m, nil
	}
	if msg.Paste {
		return m.attachImages(i, string(msg.Runes)), nil // dragged/pasted path(s)
	}
	switch msg.Type {
	case tea.KeyEnter:
		if strings.TrimSpace(m.runText) != "" {
			m = m.attachImages(i, m.runText)
			m.runText = ""
		} else {
			m.runImgPrompt = -1
			// Content tickets: advance to the stack picker.
			if m.runTickets[i].kind == "content" {
				return m.openStackPicker(i)
			}
		}
	case tea.KeyEsc:
		m.runImgPrompt, m.runText = -1, ""
		if i < len(m.runTickets) && m.runTickets[i].kind == "content" {
			return m.openStackPicker(i)
		}
	case tea.KeyBackspace:
		if r := []rune(m.runText); len(r) > 0 {
			m.runText = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		m.runText += " "
	case tea.KeyRunes:
		m.runText += string(msg.Runes)
	default:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m monitorModel) attachImages(i int, s string) monitorModel {
	for _, p := range parseDroppedPaths(s) {
		if isImageFile(p) {
			m.runTickets[i].images = append(m.runTickets[i].images, p)
		}
	}
	return m
}

// defaultBaseSentinel marks the picker's first row - "use the repo's default
// base, don't stack". It's rendered as the actual default branch name (via
// baseLabel); the leading NUL keeps it from ever colliding with a real branch.
const defaultBaseSentinel = "\x00default"

// baseLabel is the display text for a picker row. The sentinel row shows the
// repo's default branch (e.g. "main (default)") instead of a cryptic "none".
func (m monitorModel) baseLabel(b git.Branch) string {
	if b.Name == defaultBaseSentinel {
		if m.stackDefault != "" {
			return m.stackDefault + " (default)"
		}
		return "default base"
	}
	return b.Name
}

// openStackPicker loads branch list and enters the stack sub-step for chip i.
func (m monitorModel) openStackPicker(i int) (tea.Model, tea.Cmd) {
	repoPath := ""
	if cfg, err := config.Load(); err == nil && len(cfg.Repos) > 0 {
		repoPath = cfg.Repos[0].Path
	}
	branches, _ := git.Branches(repoPath) // best-effort; empty list = picker is empty
	m.stackBranches = branches
	m.stackDefault = git.DefaultBranch(repoPath)
	m.stackFilter = ""
	m.stackCursor = 0
	m.runStackPrompt = i
	return m, nil
}

// filteredBranches returns the picker list: the default-base row followed by
// branches whose name contains the current filter (case-insensitive).
func (m monitorModel) filteredBranches() []git.Branch {
	none := git.Branch{Name: defaultBaseSentinel}
	if m.stackFilter == "" {
		return append([]git.Branch{none}, m.stackBranches...)
	}
	f := strings.ToLower(m.stackFilter)
	out := []git.Branch{none}
	for _, b := range m.stackBranches {
		if strings.Contains(strings.ToLower(b.Name), f) {
			out = append(out, b)
		}
	}
	return out
}

func (m monitorModel) updateRunStack(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	i := m.runStackPrompt
	if i < 0 || i >= len(m.runTickets) {
		m.runStackPrompt = -1
		return m, nil
	}
	isContentTicket := i < len(m.runTickets) && m.runTickets[i].kind == "content"
	list := m.filteredBranches()
	switch msg.Type {
	case tea.KeyEnter:
		if m.stackCursor < len(list) {
			b := list[m.stackCursor]
			if b.Name != defaultBaseSentinel {
				m.runTickets[i].base = b.Name
			}
		}
		m.runStackPrompt, m.stackFilter, m.stackCursor = -1, "", 0
		// Content tickets: advance to the final plan-review toggle step (the last
		// in the finalize chain), which launches on its Enter.
		if isContentTicket {
			return m.openReviewPicker(i)
		}
	case tea.KeyEsc:
		// Esc means "cancel the creation", not "launch with the default base".
		// For content (the stack step is the last in a linear finalize chain)
		// abort the whole creation. For jira/file the picker was opened via
		// ctrl+s on an existing chip list, so just close it and keep the chips.
		m.runStackPrompt, m.stackFilter, m.stackCursor = -1, "", 0
		if isContentTicket {
			return m.cancelRunInput(), nil
		}
	case tea.KeyUp:
		if m.stackCursor > 0 {
			m.stackCursor--
		}
	case tea.KeyDown:
		if m.stackCursor < len(list)-1 {
			m.stackCursor++
		}
	case tea.KeyBackspace:
		if r := []rune(m.stackFilter); len(r) > 0 {
			m.stackFilter = string(r[:len(r)-1])
			m.stackCursor = 0
		}
	case tea.KeyRunes:
		m.stackFilter += string(msg.Runes)
		m.stackCursor = 0
	default:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m monitorModel) renderRunStack(w int) string {
	var b strings.Builder

	// Show existing chips first so the context is always visible.
	for _, t := range m.runTickets {
		b.WriteString("  " + chipLabel(t) + "\n")
	}
	if len(m.runTickets) > 0 {
		b.WriteString("\n")
	}

	b.WriteString(headerStyle.Render("  Choose the base branch if this ticket depends on another") + "\n")
	b.WriteString(dimStyle.Render("  the PR will target this branch - pick the default to not stack, esc cancels") + "\n\n")

	// Search box: the user types to filter the branch list below.
	b.WriteString(headerStyle.Render("  Search branches") + "\n")
	placeholder := ""
	if m.stackFilter == "" {
		placeholder = dimStyle.Render("type to search…")
	}
	b.WriteString("  🔍 " + m.stackFilter + "▌ " + placeholder + "\n\n")
	list := m.filteredBranches()
	maxShow := 12
	start := 0
	if m.stackCursor >= maxShow {
		start = m.stackCursor - maxShow + 1
	}
	for idx := start; idx < len(list) && idx < start+maxShow; idx++ {
		br := list[idx]
		name := m.baseLabel(br)
		tag := ""
		if br.Remote {
			tag = dimStyle.Render(" (remote)")
		} else if br.Name != defaultBaseSentinel {
			tag = dimStyle.Render(" (local)")
		}
		line := "   " + name + tag
		if idx == m.stackCursor {
			line = selStyle.Render(" "+name) + tag
		}
		b.WriteString(truncate(line, w) + "\n")
	}
	if len(list) == 1 { // only the "none" row survived the filter
		b.WriteString("  " + dimStyle.Render("(no branches match your search)") + "\n")
	}
	b.WriteString("\n  " + dimStyle.Render("type to search · ↑↓ move · enter select · esc cancel"))
	return b.String()
}

// openReviewPicker enters the final plan-review toggle step for chip i, with the
// cursor pre-selected from the config default (0 = Yes/pause, 1 = No/run through).
func (m monitorModel) openReviewPicker(i int) (tea.Model, tea.Cmd) {
	m.runReviewPrompt = i
	m.reviewCursor = 1 // default: No (run straight through)
	if cfg, err := config.Load(); err == nil && cfg.ReviewPlans {
		m.reviewCursor = 0 // config opts in → pre-select Yes
	}
	return m, nil
}

// updateRunReview handles the two-item "Review the plan before implementing?"
// mini palette. ↑↓ moves; Enter records the choice on the chip and launches; Esc
// cancels the whole creation (consistent with the stack step for content tickets).
func (m monitorModel) updateRunReview(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	i := m.runReviewPrompt
	if i < 0 || i >= len(m.runTickets) {
		m.runReviewPrompt = -1
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		m.runTickets[i].reviewPlan = m.reviewCursor == 0
		m.runReviewPrompt, m.reviewCursor = -1, 0
		return m.launchOrClose()
	case tea.KeyEsc:
		return m.cancelRunInput(), nil
	case tea.KeyUp:
		m.reviewCursor = 0
	case tea.KeyDown:
		m.reviewCursor = 1
	default:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m monitorModel) renderRunReview(w int) string {
	var b strings.Builder

	// Show existing chips first so the context stays visible.
	for _, t := range m.runTickets {
		b.WriteString("  " + chipLabel(t) + "\n")
	}
	if len(m.runTickets) > 0 {
		b.WriteString("\n")
	}

	b.WriteString(headerStyle.Render("  Review the plan before implementing?") + "\n\n")
	items := []string{
		"Yes — pause so I can approve or give feedback",
		"No — run straight through",
	}
	for i, item := range items {
		if i == m.reviewCursor {
			b.WriteString(selStyle.Render(" "+item) + "\n")
		} else {
			b.WriteString("  " + item + "\n")
		}
	}
	b.WriteString("\n  " + dimStyle.Render("↑↓ move · enter launch · esc cancel"))
	return b.String()
}

// chipLabel renders a pending ticket chip label (shared by renderRunInput and renderRunStack).
func chipLabel(t pendingTicket) string {
	if t.kind == "jira" {
		lbl := fmt.Sprintf("[%s]", t.id)
		if t.base != "" {
			lbl = fmt.Sprintf("[%s ⤷ %s]", t.id, t.base)
		}
		return lbl
	}
	id := t.id
	if id == "" {
		id = "?"
	}
	suffix := imgSuffix(len(t.images))
	if t.base != "" {
		suffix += " ⤷ " + t.base
	}
	return fmt.Sprintf("[%s · %s · %d line%s%s]",
		id, truncate(stripIDPrefix(t.title, t.id), 40), t.lines, plural(t.lines), suffix)
}

// updateRunIDPrompt: confirm/fix the pre-filled id for a pasted content ticket.
// On accept it advances to the image-attach step. The chip's id doubles as buffer.
func (m monitorModel) updateRunIDPrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	i := m.runIDPrompt
	if i < 0 || i >= len(m.runTickets) {
		m.runIDPrompt = -1
		return m, nil
	}
	if msg.Paste {
		return m, nil // ignore pastes while editing the id
	}
	switch msg.Type {
	case tea.KeyEnter:
		if id := normalizeTicket(m.runTickets[i].id); id != "" {
			m.runTickets[i].id = id
			m.runIDPrompt = -1
			m.runImgPrompt = i // next: attach images
		}
	case tea.KeyEsc:
		m.runTickets = append(m.runTickets[:i], m.runTickets[i+1:]...) // drop this chip
		m.runIDPrompt = -1
	case tea.KeyBackspace:
		if r := []rune(m.runTickets[i].id); len(r) > 0 {
			m.runTickets[i].id = string(r[:len(r)-1])
		}
	case tea.KeyRunes:
		m.runTickets[i].id += string(msg.Runes)
	default:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m monitorModel) renderRunInput(w int) string {
	if m.runStackPrompt >= 0 {
		return m.renderRunStack(w)
	}
	if m.runReviewPrompt >= 0 {
		return m.renderRunReview(w)
	}
	var b strings.Builder
	chips := func() {
		for i, t := range m.runTickets {
			label := chipLabel(t)
			if i == m.runIDPrompt || i == m.runImgPrompt {
				label = selStyle.Render(" " + label + " ")
			}
			b.WriteString("  " + label + "\n")
		}
		if len(m.runTickets) > 0 {
			b.WriteString("\n")
		}
	}

	if m.runIDPrompt >= 0 && m.runIDPrompt < len(m.runTickets) {
		b.WriteString(headerStyle.Render("  Confirm the ticket id") + "\n")
		b.WriteString(dimStyle.Render("  fix it if it grabbed the epic, not the ticket") + "\n\n")
		chips()
		b.WriteString(renderBodyPreview(m.runTickets[m.runIDPrompt].body, w))
		b.WriteString("  ticket id › " + m.runTickets[m.runIDPrompt].id + "▌\n")
		b.WriteString("\n  " + dimStyle.Render("enter next · esc drop this ticket"))
		return b.String()
	}
	if m.runImgPrompt >= 0 && m.runImgPrompt < len(m.runTickets) {
		n := len(m.runTickets[m.runImgPrompt].images)
		chips()
		b.WriteString(renderBodyPreview(m.runTickets[m.runImgPrompt].body, w))
		b.WriteString(headerStyle.Render("  Attach images (optional)") + "\n")
		b.WriteString(dimStyle.Render("  drag image files into the terminal, then enter") + "\n\n")
		b.WriteString("  › " + m.runText + "▌\n")
		b.WriteString("\n  " + dimStyle.Render(fmt.Sprintf("%d attached · enter done · esc skip", n)))
		return b.String()
	}

	switch m.runMode {
	case "jira":
		b.WriteString(headerStyle.Render("  From Jira") + "\n")
		b.WriteString(dimStyle.Render("  type/paste Jira key(s); space adds more") + "\n\n")
		chips()
		b.WriteString("  › " + m.runText + "▌\n")
		b.WriteString("\n  " + dimStyle.Render("space add · ctrl+s stack · enter launch · esc cancel"))
	default: // content
		b.WriteString(headerStyle.Render("  Paste ticket content") + "\n")
		b.WriteString(dimStyle.Render("  paste a ticket; you'll confirm its id and attach images") + "\n\n")
		chips()
		b.WriteString("  " + dimStyle.Render("paste a ticket · enter launch · esc cancel"))
	}
	return b.String()
}

// launchRun spawns `agent run <args…>` detached. When any ticket has a stack
// base set, each ticket gets its own subprocess (so --base can be per-ticket);
// otherwise all tickets share one batched call.
func (m monitorModel) launchRun(tickets []pendingTicket) tea.Cmd {
	self := m.selfPath
	return func() tea.Msg {
		// A per-ticket base OR a per-ticket plan-review flag forces the per-ticket
		// subprocess path (batching one `run` call can't carry per-chip flags).
		anyStacked := false
		for _, t := range tickets {
			if t.base != "" || t.reviewPlan {
				anyStacked = true
				break
			}
		}

		ticketArg := func(t pendingTicket) (string, error) {
			switch t.kind {
			case "content":
				return writePastedTicket(t.id, t.body, t.images)
			default:
				return t.id, nil
			}
		}

		if anyStacked {
			// One subprocess per ticket so --base can differ per chip.
			for _, t := range tickets {
				arg, err := ticketArg(t)
				if err != nil {
					return runLaunchedMsg{err: err}
				}
				cmdArgs := []string{"run", arg}
				if t.base != "" {
					cmdArgs = append(cmdArgs, "--base", t.base)
				}
				if t.reviewPlan {
					cmdArgs = append(cmdArgs, "--review-plan")
				}
				c := exec.Command(self, cmdArgs...)
				c.Stdout, c.Stderr = nil, nil
				c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				if err := c.Start(); err != nil {
					return runLaunchedMsg{err: err}
				}
			}
			return runLaunchedMsg{text: fmt.Sprintf("%d ticket(s)", len(tickets))}
		}

		// Batch: no stacking, one process.
		args := []string{"run"}
		for _, t := range tickets {
			arg, err := ticketArg(t)
			if err != nil {
				return runLaunchedMsg{err: err}
			}
			args = append(args, arg)
		}
		if len(args) == 1 {
			return runLaunchedMsg{err: fmt.Errorf("no tickets to launch")}
		}
		c := exec.Command(self, args...)
		c.Stdout, c.Stderr = nil, nil
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		err := c.Start()
		return runLaunchedMsg{text: fmt.Sprintf("%d ticket(s)", len(tickets)), err: err}
	}
}

// spawnRun launches `agent run <args…>` detached with pre-split args (no
// whitespace splitting, so paths with spaces survive). Used for resume/ship/rerun
// of an existing ticket, where args are a ticket key/path plus optional flags.
func (m monitorModel) spawnRun(args ...string) tea.Cmd {
	self := m.selfPath
	full := append([]string{"run"}, args...)
	return func() tea.Msg {
		c := exec.Command(self, full...)
		c.Stdout, c.Stderr = nil, nil
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		err := c.Start()
		return runLaunchedMsg{text: strings.Join(args, " "), err: err}
	}
}

// writePastedTicket saves a pasted ticket to ~/.magneton/pasted/<id>/<id>.md (so
// ticketIDFromPath recovers <id>) and copies its images beside it. Returns the
// .md path for `agent run`. Images are copied (not referenced) so a later
// move/delete of the user's original screenshot can't break the run.
func writePastedTicket(id, body string, images []string) (string, error) {
	if err := paths.EnsureDirs(); err != nil {
		return "", err
	}
	if id == "" {
		id = "PASTED"
	}
	dir := filepath.Join(paths.Pasted(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	mdPath := filepath.Join(dir, id+".md")
	if err := os.WriteFile(mdPath, []byte(body), 0o644); err != nil {
		return "", err
	}
	for i, src := range images {
		data, err := os.ReadFile(src)
		if err != nil {
			return "", fmt.Errorf("read image %s: %w", src, err)
		}
		dst := filepath.Join(dir, fmt.Sprintf("img-%d%s", i+1, strings.ToLower(filepath.Ext(src))))
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return "", fmt.Errorf("write image %s: %w", dst, err)
		}
	}
	return mdPath, nil
}

// parseDroppedPaths splits terminal-inserted file paths, honoring single/double
// quotes and backslash-escaped spaces (how a terminal encodes a dragged path).
func parseDroppedPaths(s string) []string {
	var out []string
	var cur strings.Builder
	inSingle, inDouble, esc := false, false, false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && !inSingle:
			esc = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case (r == ' ' || r == '\t' || r == '\n' || r == '\r') && !inSingle && !inDouble:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// isImageFile reports whether p is an existing file with an image extension.
func isImageFile(p string) bool {
	if !isImageExt(p) {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// renderBodyPreview renders the first 12 lines of a pasted ticket body as a dim
// quoted block so the user can see what they pasted while confirming its id and
// attaching images (paste is blind otherwise - two corruption bugs slipped
// through invisibly). Returns "" for an empty body (jira chips never have one).
func renderBodyPreview(body string, w int) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	const maxLines = 12
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	var b strings.Builder
	for i, ln := range lines {
		if i >= maxLines {
			break
		}
		b.WriteString(dimStyle.Render("  │ "+truncate(ln, w-4)) + "\n")
	}
	if n := len(lines) - maxLines; n > 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  │ … +%d more lines", n)) + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

// lineCount returns the number of lines in s, ignoring a single trailing newline.
func lineCount(s string) int {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func imgSuffix(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" · %d img", n)
}

// ---- doctor output ---------------------------------------------------------

func (m monitorModel) runDoctor() tea.Cmd {
	self := m.selfPath
	return func() tea.Msg {
		out, _ := exec.Command(self, "doctor").CombinedOutput()
		return doctorDoneMsg{out: string(out)}
	}
}

func (m monitorModel) updateOutput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "enter":
		m.view = viewDashboard
	case "ctrl+c":
		return m, tea.Quit
	}
	return m, nil
}

func (m monitorModel) renderOutput(w int) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("  "+m.outputTitle) + "\n\n")
	for _, ln := range strings.Split(strings.TrimRight(m.outputText, "\n"), "\n") {
		for _, seg := range wrapLine(ln, w) {
			b.WriteString(seg + "\n")
		}
	}
	b.WriteString("\n  " + dimStyle.Render("esc/enter close"))
	return b.String()
}

// ---- daemon control --------------------------------------------------------

// daemonAlive reports the daemon pid and whether it's running (reuses start.go).
func daemonAlive() (int, bool) {
	if pid, ok := readPid(); ok && processAlive(pid) {
		return pid, true
	}
	return 0, false
}

func (m monitorModel) startDaemon() tea.Cmd {
	self := m.selfPath
	return func() tea.Msg {
		c := exec.Command(self, "start")
		c.Stdout, c.Stderr = nil, nil
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return daemonMsg{action: "start", err: c.Start()}
	}
}

func (m monitorModel) stopDaemon() tea.Cmd {
	return func() tea.Msg {
		pid, ok := daemonAlive()
		if !ok {
			return daemonMsg{action: "stop", err: fmt.Errorf("daemon not running")}
		}
		return daemonMsg{action: "stop", err: syscall.Kill(pid, syscall.SIGTERM)}
	}
}

// ---- config / setup forms --------------------------------------------------

func (m monitorModel) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.form == nil {
		m.view = viewDashboard
		return m, nil
	}
	// Pasted text: drop it in at the caret as a single line (config fields are
	// single-line, so collapse any newlines).
	if msg.Paste {
		m.form.typeRunes(strings.ReplaceAll(normalizeNewlines(string(msg.Runes)), "\n", " "))
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEsc:
		m.view = viewDashboard
		m.form = nil
	case tea.KeyEnter:
		submit, fields := m.form.submit, m.form.fields
		m.view = viewDashboard
		m.form = nil
		if submit != nil {
			return m, func() tea.Msg { return submit(fields) }
		}
	case tea.KeyTab, tea.KeyDown:
		m.form.next()
	case tea.KeyShiftTab, tea.KeyUp:
		m.form.prev()
	case tea.KeyLeft:
		m.form.left()
	case tea.KeyRight:
		m.form.right()
	case tea.KeyHome:
		m.form.home()
	case tea.KeyEnd:
		m.form.end()
	case tea.KeyBackspace:
		m.form.backspace()
	case tea.KeyDelete:
		m.form.deleteForward()
	case tea.KeySpace:
		m.form.typeRunes(" ")
	case tea.KeyRunes:
		m.form.typeRunes(string(msg.Runes))
	default:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

// configFields builds the editable (non-secret) field set from a config.
//
// Models are free-text on purpose: the set of usable model identifiers depends
// on the account (personal vs enterprise) and the org's policy, and Claude Code
// fetches that list at runtime - there's no offline list magneton could show
// without it going stale or offering models a company forbids. Leave a model
// blank to inherit Claude Code's configured default; or type the exact id your
// account allows (e.g. "sonnet", "claude-opus-4-8", or a Bedrock/Vertex id).
func configFields(cfg *config.Config) []formField {
	repo := config.Repo{}
	if len(cfg.Repos) > 0 {
		repo = cfg.Repos[0]
	}
	return []formField{
		{label: "Repository path for the Android app", value: repo.Path},
		{label: "Branch name pattern", value: repo.Branch},
		{label: "Base branch to open PRs against (blank = repo default)", value: repo.Base},
		{label: "Model for the planning stage (blank = Claude Code default)", value: cfg.ModelPlan},
		{label: "Model for the implementation stage (blank = default)", value: cfg.ModelImpl},
		{label: "Model for the self-review stage (blank = default)", value: cfg.ModelReview},
		{label: "Pause to review each plan before coding? (y/n)", value: boolToYN(cfg.ReviewPlans)},
	}
}

// boolToYN / parseYN render and parse the loose y/n form field for booleans.
func boolToYN(v bool) string {
	if v {
		return "y"
	}
	return "n"
}

func parseYN(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes", "true", "1":
		return true
	}
	return false
}

// applyConfigFields writes the (non-secret) form values back onto a config.
// Jira settings are intentionally not shown/edited here; any values already in
// the config file are preserved untouched.
func applyConfigFields(cfg *config.Config, f []formField) {
	repo := config.Repo{}
	if len(cfg.Repos) > 0 {
		repo = cfg.Repos[0]
	}
	repo.Path = f[0].value
	repo.Branch = f[1].value
	repo.Base = f[2].value
	cfg.ModelPlan = f[3].value
	cfg.ModelImpl = f[4].value
	cfg.ModelReview = f[5].value
	cfg.ReviewPlans = parseYN(f[6].value)
	cfg.Repos = []config.Repo{repo}
}

func (m *monitorModel) openConfigForm() {
	cfg, err := config.Load()
	if err != nil {
		// No config yet → fall back to the setup wizard.
		m.openSetupForm()
		return
	}
	m.form = &formModel{
		title:  "Edit config",
		note:   "~/.magneton/config.toml · first repo",
		fields: configFields(cfg),
		submit: func(f []formField) tea.Msg {
			cfg, err := config.Load()
			if err != nil {
				cfg = &config.Config{}
			}
			applyConfigFields(cfg, f)
			if err := config.Save(cfg); err != nil {
				return formDoneMsg{err: err}
			}
			return formDoneMsg{notice: "config saved"}
		},
	}
	m.form.focusEnd()
	m.view = viewForm
}

func (m *monitorModel) openSetupForm() {
	cfg, err := config.Load()
	if err != nil {
		cfg = &config.Config{
			Concurrency: 3, PollInterval: 30, MaxBudgetUSD: 5,
			Repos: []config.Repo{{
				Path: "~/src/android-app", Branch: "{ticket}-{slug}",
			}},
		}
	}
	fields := append(configFields(cfg),
		formField{label: "Anthropic API key (blank = skip, saved to keychain)", secret: true},
	)
	m.form = &formModel{
		title:  "Setup wizard",
		note:   "writes ~/.magneton/config.toml; tokens go to the OS keychain",
		fields: fields,
		submit: func(f []formField) tea.Msg {
			cfg, err := config.Load()
			if err != nil {
				cfg = &config.Config{PollInterval: 30, Concurrency: 3, MaxBudgetUSD: 5}
			}
			n := len(f) - 1 // last field is the secret Anthropic key
			applyConfigFields(cfg, f[:n])
			if err := config.Save(cfg); err != nil {
				return formDoneMsg{err: err}
			}
			if key := strings.TrimSpace(f[n].value); key != "" {
				_ = secrets.Set(secrets.Anthropic, key)
			}
			return formDoneMsg{notice: "setup saved - pick Doctor to verify"}
		},
	}
	m.form.focusEnd()
	m.view = viewForm
}
