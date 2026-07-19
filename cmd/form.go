package cmd

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// formField is one editable line in a form. secret fields render masked and are
// meant for keychain storage, never the config file. cursor is the rune index
// of the edit caret within value (0..len(value)).
type formField struct {
	label  string
	value  string
	secret bool
	cursor int
}

// formModel is a minimal hand-rolled multi-field form (no bubbles dependency).
// submit receives the filled fields and returns a tea.Msg with the result.
type formModel struct {
	title  string
	note   string
	fields []formField
	focus  int
	submit func(fields []formField) tea.Msg
}

// clampCursor keeps the focused field's caret within [0, len(value)] and returns
// the field's runes and the clamped position.
func (f *formModel) caret() (*formField, []rune, int) {
	fld := &f.fields[f.focus]
	r := []rune(fld.value)
	if fld.cursor < 0 {
		fld.cursor = 0
	}
	if fld.cursor > len(r) {
		fld.cursor = len(r)
	}
	return fld, r, fld.cursor
}

// typeRunes inserts s at the caret and advances it.
func (f *formModel) typeRunes(s string) {
	fld, r, pos := f.caret()
	ins := []rune(s)
	out := make([]rune, 0, len(r)+len(ins))
	out = append(out, r[:pos]...)
	out = append(out, ins...)
	out = append(out, r[pos:]...)
	fld.value = string(out)
	fld.cursor = pos + len(ins)
}

// backspace deletes the rune before the caret.
func (f *formModel) backspace() {
	fld, r, pos := f.caret()
	if pos > 0 {
		fld.value = string(r[:pos-1]) + string(r[pos:])
		fld.cursor = pos - 1
	}
}

// deleteForward deletes the rune at the caret (the Delete key).
func (f *formModel) deleteForward() {
	fld, r, pos := f.caret()
	if pos < len(r) {
		fld.value = string(r[:pos]) + string(r[pos+1:])
	}
}

func (f *formModel) left() {
	if _, _, pos := f.caret(); pos > 0 {
		f.fields[f.focus].cursor = pos - 1
	}
}

func (f *formModel) right() {
	if _, r, pos := f.caret(); pos < len(r) {
		f.fields[f.focus].cursor = pos + 1
	}
}

func (f *formModel) home() { f.fields[f.focus].cursor = 0 }
func (f *formModel) end()  { f.fields[f.focus].cursor = len([]rune(f.fields[f.focus].value)) }

// focusEnd places the caret at the end of the focused field - the natural spot
// when a field gains focus (e.g. on open or after tab).
func (f *formModel) focusEnd() {
	f.fields[f.focus].cursor = len([]rune(f.fields[f.focus].value))
}

func (f *formModel) next() {
	if f.focus < len(f.fields)-1 {
		f.focus++
		f.focusEnd()
	}
}

func (f *formModel) prev() {
	if f.focus > 0 {
		f.focus--
		f.focusEnd()
	}
}

func (f *formModel) render(w int) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("  "+f.title) + "\n")
	if f.note != "" {
		b.WriteString(dimStyle.Render("  "+f.note) + "\n")
	}
	b.WriteString("\n")
	for i, fld := range f.fields {
		val := fld.value
		if fld.secret {
			val = strings.Repeat("•", len([]rune(fld.value)))
		}
		if i == f.focus {
			// Draw the caret inside the value at the cursor position.
			r := []rune(val)
			pos := fld.cursor
			if pos < 0 {
				pos = 0
			}
			if pos > len(r) {
				pos = len(r)
			}
			val = string(r[:pos]) + "▌" + string(r[pos:])
			line := fmt.Sprintf("  %-20s %s", fld.label, val)
			b.WriteString(truncate(selStyle.Render(line), w) + "\n")
			continue
		}
		line := fmt.Sprintf("  %-20s %s", fld.label, val)
		b.WriteString(truncate(line, w) + "\n")
	}
	b.WriteString("\n  " + dimStyle.Render("tab/↑↓ move · ←→ edit · type to change · enter save · esc cancel"))
	return b.String()
}
