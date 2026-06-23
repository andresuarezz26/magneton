package cmd

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// formField is one editable line in a form. secret fields render masked and are
// meant for keychain storage, never the config file. options turns the field
// into a cycle-picker (left/right arrows) instead of free text.
type formField struct {
	label   string
	value   string
	secret  bool
	options []string
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

func (f *formModel) typeRunes(s string) {
	if len(f.fields[f.focus].options) > 0 {
		return
	}
	f.fields[f.focus].value += s
}

func (f *formModel) backspace() {
	if len(f.fields[f.focus].options) > 0 {
		return
	}
	if r := []rune(f.fields[f.focus].value); len(r) > 0 {
		f.fields[f.focus].value = string(r[:len(r)-1])
	}
}

func (f *formModel) cycleNext() {
	fld := &f.fields[f.focus]
	if len(fld.options) == 0 {
		return
	}
	for i, o := range fld.options {
		if o == fld.value {
			fld.value = fld.options[(i+1)%len(fld.options)]
			return
		}
	}
	fld.value = fld.options[0]
}

func (f *formModel) cyclePrev() {
	fld := &f.fields[f.focus]
	if len(fld.options) == 0 {
		return
	}
	for i, o := range fld.options {
		if o == fld.value {
			fld.value = fld.options[(i-1+len(fld.options))%len(fld.options)]
			return
		}
	}
	fld.value = fld.options[0]
}

func (f *formModel) next() {
	if f.focus < len(f.fields)-1 {
		f.focus++
	}
}

func (f *formModel) prev() {
	if f.focus > 0 {
		f.focus--
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
		var line string
		if len(fld.options) > 0 {
			inner := fmt.Sprintf("  %-20s ◀ %s ▶", fld.label, val)
			if i == f.focus {
				line = selStyle.Render(inner)
			} else {
				line = inner
			}
		} else {
			line = fmt.Sprintf("  %-20s %s", fld.label, val)
			if i == f.focus {
				line = selStyle.Render(line + "▌")
			}
		}
		b.WriteString(truncate(line, w) + "\n")
	}
	b.WriteString("\n  " + dimStyle.Render("tab/↑↓ move · ←→ cycle options · type to edit · enter save · esc cancel"))
	return b.String()
}
