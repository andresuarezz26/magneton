package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/git"
	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/runner"
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

// ticketEditedMsg returns from the external-editor session opened with ctrl+e
// on the branch prompt: the pasted ticket body was (maybe) rewritten at path.
type ticketEditedMsg struct {
	i    int    // index of the chip being edited
	path string // temp .md file the editor was opened on
	err  error
}

// pendingTicket is one queued ticket in the "Start new ticket(s)" input, shown
// as a chip. kind is content|jira.
type pendingTicket struct {
	id         string // detected ticket id (content kind: auto-inferred, not user-edited)
	title      string // parsed title (or the key itself, for a jira chip)
	lines      int
	kind       string   // "content" | "jira"
	body       string   // raw pasted content (content kind)
	images     []string // attached image files (content kind)
	base       string   // chosen base branch name (bare); "" = default
	branch     string   // final branch name (pre-filled from the pattern; also the edit buffer while prompting)
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
	ms := []runMethod{{"content", "Write or paste the ticket content", "a markdown editor - then confirm its id + branch"}}
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
			m.runText, m.runTickets, m.runIDPrompt, m.runBranchPrompt, m.runImgPrompt = "", nil, -1, -1, -1
			m.promptCursor = 0
			m.view = viewRunInput
			if m.runMode == "content" {
				m = m.withContentEditor()
				return m, textarea.Blink
			}
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
	if m.runBranchPrompt >= 0 {
		return m.updateRunBranchPrompt(msg)
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
	m.runIDPrompt, m.runBranchPrompt, m.runImgPrompt, m.runStackPrompt = -1, -1, -1, -1
	m.runReviewPrompt, m.reviewCursor = -1, 0
	m.stackBranches, m.stackDefault, m.stackFilter, m.stackCursor = nil, "", "", 0
	m.ticketLines, m.ticketScroll = nil, 0
	m.content, m.contentPreview = nil, false
	m.contentBar, m.contentBtn = false, 0
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

// content method: a real multi-line markdown EDITOR - the ticket content drives
// the whole run, so composing it well matters. Enter inserts a new line. Esc
// steps OUT of the editor onto an always-visible action bar (Continue / Preview
// / Discard) below the viewport - so confirming never needs scrolling and a
// stray Esc can't destroy the draft. ctrl+d (quick confirm) and ctrl+p
// (preview) remain as shortcuts. Pastes land in the editor for review instead
// of becoming a chip directly.
func (m monitorModel) updateRunContent(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.content == nil { // safety net; entry points arm the editor
		m = m.withContentEditor()
	}

	// Preview mode: read-only - scroll, flip back to editing, or confirm.
	if m.contentPreview {
		switch msg.Type {
		case tea.KeyEsc:
			return m.backToEditing()
		case tea.KeyUp:
			m.ticketScroll--
			m.clampTicketScroll()
		case tea.KeyDown:
			m.ticketScroll++
			m.clampTicketScroll()
		case tea.KeyPgUp:
			m.ticketScroll -= m.ticketViewportHeight()
			m.clampTicketScroll()
		case tea.KeyPgDown:
			m.ticketScroll += m.ticketViewportHeight()
			m.clampTicketScroll()
		default:
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "ctrl+p":
				return m.backToEditing()
			case "ctrl+d":
				return m.confirmContent()
			}
		}
		return m, nil
	}

	// Action bar focused: ←→ choose, Enter activates, Esc back into the editor.
	if m.contentBar {
		switch msg.Type {
		case tea.KeyEsc, tea.KeyTab:
			return m.backToEditing()
		case tea.KeyLeft:
			if m.contentBtn > 0 {
				m.contentBtn--
			}
		case tea.KeyRight:
			if m.contentBtn < 2 {
				m.contentBtn++
			}
		case tea.KeyEnter:
			switch m.contentBtn {
			case 0: // Continue to next step
				return m.confirmContent()
			case 1: // Keep editing
				return m.backToEditing()
			case 2: // Discard
				return m.cancelRunInput(), nil
			}
		default:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
		}
		return m, nil
	}

	if msg.Paste {
		m.content.InsertString(normalizeNewlines(string(msg.Runes)))
		return m, nil
	}
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		// Step out to the action bar - never silently discard the draft.
		m.contentBar, m.contentBtn = true, 0
		m.content.Blur()
		return m, nil
	case "ctrl+d":
		return m.confirmContent()
	case "ctrl+p":
		return m.openContentPreview()
	}
	// Backspace in an empty editor removes the last queued chip.
	if msg.Type == tea.KeyBackspace && m.content.Value() == "" {
		if n := len(m.runTickets); n > 0 {
			m.runTickets = m.runTickets[:n-1]
			return m, nil
		}
	}
	var cmd tea.Cmd
	*m.content, cmd = m.content.Update(msg)
	return m, cmd
}

// backToEditing returns focus to the editor from the action bar or preview.
func (m monitorModel) backToEditing() (tea.Model, tea.Cmd) {
	m.contentPreview, m.contentBar = false, false
	m.content.Focus()
	return m, textarea.Blink
}

// openContentPreview renders the draft to a scrollable glamour preview (no-op
// on an empty draft).
func (m monitorModel) openContentPreview() (tea.Model, tea.Cmd) {
	if strings.TrimSpace(m.content.Value()) == "" {
		return m, nil
	}
	m.ticketLines = renderMarkdownLines(m.content.Value(), m.paneWidth())
	m.ticketScroll = 0
	m.contentPreview = true
	return m, nil
}

// confirmContent turns the editor's content into a pending ticket, entering the
// id/branch confirm chain; with an empty editor it launches the queued chips.
func (m monitorModel) confirmContent() (tea.Model, tea.Cmd) {
	body := m.content.Value()
	if strings.TrimSpace(body) == "" {
		return m.launchOrClose()
	}
	m.content.Reset()
	m.content.Focus() // clean editing state for when the chip chain returns here
	m.contentPreview, m.contentBar, m.contentBtn = false, false, 0
	return m.addContentTicket(body)
}

// withContentEditor arms content mode's multi-line markdown editor
// (bubbles/textarea: enter for new lines, full 2D cursor movement, wrapping).
func (m monitorModel) withContentEditor() monitorModel {
	ta := textarea.New()
	ta.Placeholder = "Type or paste the ticket here - markdown welcome…"
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.ShowLineNumbers = false
	ta.Focus()
	m.content = &ta
	m.contentPreview = false
	m.sizeContentEditor()
	return m
}

// sizeContentEditor fits the editor to the terminal, leaving room for the
// screen chrome and any queued chips.
func (m monitorModel) sizeContentEditor() {
	if m.content == nil {
		return
	}
	w := m.width - 6
	if w < 40 {
		w = 40
	}
	h := m.height - 12 - len(m.runTickets) // chrome + the esc hint above and action bar below
	if h < 5 {
		h = 5
	}
	m.content.SetWidth(w)
	m.content.SetHeight(h)
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
	// Step 1: confirm the id. The branch pre-fill is computed on its Enter, from
	// the CONFIRMED id (see updateRunIDPrompt).
	m.runIDPrompt = len(m.runTickets) - 1
	m.promptCursor = len([]rune(guess))
	m.ticketLines = renderMarkdownLines(blob, m.paneWidth())
	m.ticketScroll = 0
	return m, nil
}

// inferBranch pre-fills the creation-time branch field: the repo's branch
// pattern expanded with the detected ticket id and title. The user edits the
// result, and whatever they confirm is passed through verbatim as the final
// branch name (via `run --branch`).
func inferBranch(id, title string) string {
	pattern := "{ticket}-{slug}"
	if cfg, err := config.Load(); err == nil && len(cfg.Repos) > 0 && cfg.Repos[0].Branch != "" {
		pattern = cfg.Repos[0].Branch
	}
	if id == "" {
		id = "PASTED" // writePastedTicket's fallback id
	}
	return runner.ResolveBranch(pattern, id, stripIDPrefix(title, id))
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
	// Content chips lead with the confirmed branch name (the user-facing identity
	// since the branch prompt replaced the id prompt); fall back to the id.
	head := t.branch
	if head == "" {
		head = t.id
	}
	if head == "" {
		head = "?"
	}
	suffix := imgSuffix(len(t.images))
	if t.base != "" {
		suffix += " ⤷ " + t.base
	}
	return fmt.Sprintf("[%s · %s · %d line%s%s]",
		head, truncate(stripIDPrefix(t.title, t.id), 40), t.lines, plural(t.lines), suffix)
}

// updateRunIDPrompt: step 1 - confirm/fix the detected ticket id for a pasted
// content ticket (it names the dashboard row, worktree, and logs). On accept it
// computes the branch pre-fill from the CONFIRMED id and advances to the branch
// prompt. The chip's id doubles as the edit buffer.
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
			m.runTickets[i].branch = inferBranch(id, m.runTickets[i].title)
			m.runIDPrompt = -1
			m.runBranchPrompt = i // step 2: confirm the branch name
			m.promptCursor = len([]rune(m.runTickets[i].branch))
		}
	case tea.KeyEsc:
		m.runTickets = append(m.runTickets[:i], m.runTickets[i+1:]...) // drop this chip
		m.runIDPrompt = -1
		m.ticketLines, m.ticketScroll = nil, 0
	case tea.KeyUp:
		m.ticketScroll--
		m.clampTicketScroll()
	case tea.KeyDown:
		m.ticketScroll++
		m.clampTicketScroll()
	case tea.KeyPgUp:
		m.ticketScroll -= m.ticketViewportHeight()
		m.clampTicketScroll()
	case tea.KeyPgDown:
		m.ticketScroll += m.ticketViewportHeight()
		m.clampTicketScroll()
	default:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "ctrl+e":
			return m.openTicketEditor(i)
		}
		// Caret editing (←→/Home/End/type/backspace/delete) via the shared
		// form-field editor.
		fld := formField{value: m.runTickets[i].id, cursor: m.promptCursor}
		if fld.editKey(msg) {
			m.runTickets[i].id, m.promptCursor = fld.value, fld.cursor
		}
	}
	return m, nil
}

// updateRunBranchPrompt: step 2 - confirm/edit the pre-filled branch name for a
// pasted content ticket. On accept it advances to the image-attach step. The
// chip's branch doubles as the edit buffer; the confirmed value is the FINAL
// branch name, passed verbatim to `run --branch`.
func (m monitorModel) updateRunBranchPrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	i := m.runBranchPrompt
	if i < 0 || i >= len(m.runTickets) {
		m.runBranchPrompt = -1
		return m, nil
	}
	if msg.Paste {
		return m, nil // ignore pastes while editing the branch
	}
	switch msg.Type {
	case tea.KeyEnter:
		// Collapse whitespace to "-" (git refuses spaces in branch names).
		if br := strings.Join(strings.Fields(m.runTickets[i].branch), "-"); br != "" {
			m.runTickets[i].branch = br
			m.runBranchPrompt = -1
			m.runImgPrompt = i // next: attach images
			m.ticketLines, m.ticketScroll = nil, 0
		}
	case tea.KeyEsc:
		m.runTickets = append(m.runTickets[:i], m.runTickets[i+1:]...) // drop this chip
		m.runBranchPrompt = -1
		m.ticketLines, m.ticketScroll = nil, 0
	case tea.KeyUp:
		m.ticketScroll--
		m.clampTicketScroll()
	case tea.KeyDown:
		m.ticketScroll++
		m.clampTicketScroll()
	case tea.KeyPgUp:
		m.ticketScroll -= m.ticketViewportHeight()
		m.clampTicketScroll()
	case tea.KeyPgDown:
		m.ticketScroll += m.ticketViewportHeight()
		m.clampTicketScroll()
	default:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "ctrl+e":
			return m.openTicketEditor(i)
		}
		// Caret editing (←→/Home/End/type/backspace/delete) via the shared
		// form-field editor.
		fld := formField{value: m.runTickets[i].branch, cursor: m.promptCursor}
		if fld.editKey(msg) {
			m.runTickets[i].branch, m.promptCursor = fld.value, fld.cursor
		}
	}
	return m, nil
}

// openTicketEditor suspends the TUI and opens the pasted ticket body in the
// user's $VISUAL/$EDITOR (vim fallback) via a temp .md file, so the full
// content can be reviewed and edited before the run launches.
func (m monitorModel) openTicketEditor(i int) (tea.Model, tea.Cmd) {
	if i < 0 || i >= len(m.runTickets) {
		return m, nil
	}
	f, err := os.CreateTemp("", "magneton-ticket-*.md")
	if err != nil {
		m.notice = "edit ticket: " + err.Error()
		return m, nil
	}
	path := f.Name()
	if _, err := f.WriteString(m.runTickets[i].body); err != nil {
		f.Close()
		os.Remove(path)
		m.notice = "edit ticket: " + err.Error()
		return m, nil
	}
	f.Close()
	parts := strings.Fields(editorCmd())
	c := exec.Command(parts[0], append(parts[1:], path)...)
	return m, tea.ExecProcess(c, func(err error) tea.Msg {
		return ticketEditedMsg{i: i, path: path, err: err}
	})
}

// editorCmd picks the user's editor: $VISUAL, then $EDITOR, then vim. The value
// may carry flags (e.g. "code -w"), so callers split it into fields.
func editorCmd() string {
	if v := os.Getenv("VISUAL"); v != "" {
		return v
	}
	if v := os.Getenv("EDITOR"); v != "" {
		return v
	}
	return "vim"
}

// applyTicketEdit folds the editor result back into the chip: body, title, line
// count, and detected id are re-derived. The branch field is re-inferred only if
// the user hadn't already customized it away from the previous inferred value.
func (m monitorModel) applyTicketEdit(msg ticketEditedMsg) (tea.Model, tea.Cmd) {
	defer os.Remove(msg.path)
	i := msg.i
	if msg.err != nil {
		m.notice = "edit ticket: " + msg.err.Error()
		return m, nil
	}
	if i < 0 || i >= len(m.runTickets) {
		return m, nil
	}
	raw, err := os.ReadFile(msg.path)
	if err != nil {
		m.notice = "edit ticket: " + err.Error()
		return m, nil
	}
	body := normalizeNewlines(string(raw))
	if strings.TrimSpace(body) == "" {
		m.notice = "edit discarded - the ticket can't be empty"
		return m, nil
	}
	t := &m.runTickets[i]
	prevInferred := inferBranch(t.id, t.title)
	title, _, err := parseTicketContent(body)
	if err != nil || title == "" {
		title = "(untitled)"
	}
	// Re-detect the id, but keep the current one (possibly hand-typed in the id
	// step) when the new content has none.
	if id, ok := detectTicketID(body); ok {
		t.id = id
	}
	t.body, t.title, t.lines = body, title, lineCount(body)
	if t.branch != "" && t.branch == prevInferred {
		t.branch = inferBranch(t.id, title)
	}
	if m.runIDPrompt == i || m.runBranchPrompt == i {
		m.ticketLines = renderMarkdownLines(body, m.paneWidth())
		m.ticketScroll = 0
		// The active field's value may have been re-derived - park the caret at
		// its end.
		if m.runIDPrompt == i {
			m.promptCursor = len([]rune(t.id))
		} else {
			m.promptCursor = len([]rune(t.branch))
		}
	}
	return m, nil
}

// renderTicketPrompt is the shared layout for the id and branch confirmation
// steps: the full ticket content, markdown-rendered (same glamour pipeline as
// the plan viewer) in a scrollable window, with the step's header and input
// field directly below it.
func (m monitorModel) renderTicketPrompt(body, header, field string, w int) string {
	var b strings.Builder
	lines := m.ticketLines
	if lines == nil && strings.TrimSpace(body) != "" {
		lines = renderMarkdownLines(body, m.paneWidth())
	}
	viewH := m.ticketViewportHeight()
	start, end := scrollWindow(len(lines), m.ticketScroll, viewH)
	for _, ln := range lines[start:end] {
		b.WriteString(ln + "\n")
	}
	b.WriteString("\n")
	b.WriteString(headerStyle.Render("  "+header) + "\n")
	b.WriteString("  " + field + "\n")
	hint := "←→ move · ctrl+e edit ticket · enter next · esc drop this ticket"
	if len(lines) > viewH {
		hint = fmt.Sprintf("%d–%d of %d · ↑↓ scroll · %s", start+1, end, len(lines), hint)
	}
	b.WriteString("\n  " + dimStyle.Render(hint))
	return b.String()
}

// scrollWindow returns the visible [start, end) bounds of an n-line buffer
// scrolled to offset with viewH visible rows.
func scrollWindow(n, offset, viewH int) (start, end int) {
	start = offset
	if start > n-viewH {
		start = n - viewH
	}
	if start < 0 {
		start = 0
	}
	end = start + viewH
	if end > n {
		end = n
	}
	return start, end
}

// ticketViewportHeight is how many rows of the pasted ticket are visible in the
// id/branch prompts: the screen minus their chrome (app header, blank, title,
// input line, and the blank + hint footer).
func (m monitorModel) ticketViewportHeight() int {
	h := m.height - 7
	if h < 3 {
		h = 3
	}
	return h
}

// clampTicketScroll keeps the branch prompt's scroll offset within [0, max].
func (m *monitorModel) clampTicketScroll() {
	max := len(m.ticketLines) - m.ticketViewportHeight()
	if max < 0 {
		max = 0
	}
	if m.ticketScroll > max {
		m.ticketScroll = max
	}
	if m.ticketScroll < 0 {
		m.ticketScroll = 0
	}
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
			if i == m.runBranchPrompt || i == m.runImgPrompt {
				label = selStyle.Render(" " + label + " ")
			}
			b.WriteString("  " + label + "\n")
		}
		if len(m.runTickets) > 0 {
			b.WriteString("\n")
		}
	}

	if m.runIDPrompt >= 0 && m.runIDPrompt < len(m.runTickets) {
		t := m.runTickets[m.runIDPrompt]
		return m.renderTicketPrompt(t.body,
			"Confirm the ticket id",
			"ticket id › "+formField{value: t.id, cursor: m.promptCursor}.caretView(), w)
	}
	if m.runBranchPrompt >= 0 && m.runBranchPrompt < len(m.runTickets) {
		t := m.runTickets[m.runBranchPrompt]
		return m.renderTicketPrompt(t.body,
			"Update or Approve the branch name",
			"branch › "+formField{value: t.branch, cursor: m.promptCursor}.caretView(), w)
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
		b.WriteString(headerStyle.Render("  Write or paste the ticket content") + "\n\n")
		chips()
		switch {
		case m.contentPreview:
			viewH := m.ticketViewportHeight()
			start, end := scrollWindow(len(m.ticketLines), m.ticketScroll, viewH)
			for _, ln := range m.ticketLines[start:end] {
				b.WriteString(ln + "\n")
			}
			hint := "ctrl+p edit · ctrl+d confirm ticket · esc back to editing"
			if len(m.ticketLines) > viewH {
				hint = fmt.Sprintf("%d–%d of %d · ↑↓ scroll · %s", start+1, end, len(m.ticketLines), hint)
			}
			b.WriteString("\n  " + dimStyle.Render(hint))
		case m.content != nil:
			// Reserved row so the editor doesn't jump when focus moves to the bar.
			escHint := ""
			if !m.contentBar {
				escHint = headerStyle.Render("  Press ESC to leave the edit mode")
			}
			b.WriteString(escHint + "\n")
			b.WriteString(m.content.View() + "\n\n")
			// Action bar: always visible below the editor viewport (the editor
			// scrolls internally, so long content never pushes it off-screen).
			buttons := []string{"Continue to next step", "Keep editing", "Discard"}
			if strings.TrimSpace(m.content.Value()) == "" && len(m.runTickets) > 0 {
				buttons[0] = "Launch" // empty editor + queued chips: Continue launches them
			}
			var bar strings.Builder
			bar.WriteString("  ")
			for i, it := range buttons {
				if m.contentBar && i == m.contentBtn {
					bar.WriteString(planBtnSel.Render("▸ "+it) + "  ")
				} else {
					bar.WriteString(planBtn.Render(it) + "  ")
				}
			}
			b.WriteString(bar.String() + "\n")
			hint := "enter new line · esc: actions · ctrl+p preview · ctrl+d quick-confirm"
			if m.contentBar {
				hint = "←→ choose · enter select · esc/tab back to editing"
			}
			b.WriteString("\n  " + dimStyle.Render(hint))
		}
	}
	return b.String()
}

// launchRun spawns `agent run <args…>` detached. When any ticket has a stack
// base set, each ticket gets its own subprocess (so --base can be per-ticket);
// otherwise all tickets share one batched call.
func (m monitorModel) launchRun(tickets []pendingTicket) tea.Cmd {
	self := m.selfPath
	return func() tea.Msg {
		// A per-ticket base, plan-review flag, or branch name forces the per-ticket
		// subprocess path (batching one `run` call can't carry per-chip flags).
		anyStacked := false
		for _, t := range tickets {
			if t.base != "" || t.reviewPlan || t.branch != "" {
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
				if t.branch != "" {
					cmdArgs = append(cmdArgs, "--branch", t.branch)
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
