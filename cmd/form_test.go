package cmd

import "testing"

// The form must support in-field cursor editing: type/backspace/delete happen at
// the caret, and ←→/Home/End move it - so a value can be edited in the middle,
// not only by deleting from the end.
func TestFormCursorEditing(t *testing.T) {
	f := &formModel{fields: []formField{{label: "Branch", value: "main"}}}
	f.focusEnd()
	if f.fields[0].cursor != 4 {
		t.Fatalf("focusEnd: cursor = %d, want 4", f.fields[0].cursor)
	}

	// Move left twice → between "ma" and "in"; insert "XY".
	f.left()
	f.left()
	f.typeRunes("XY")
	if got := f.fields[0].value; got != "maXYin" {
		t.Errorf("insert mid-value = %q, want %q", got, "maXYin")
	}
	if f.fields[0].cursor != 4 { // after the inserted "XY"
		t.Errorf("cursor after insert = %d, want 4", f.fields[0].cursor)
	}

	// Backspace deletes the rune before the caret ("Y").
	f.backspace()
	if got := f.fields[0].value; got != "maXin" {
		t.Errorf("backspace mid-value = %q, want %q", got, "maXin")
	}

	// Home, then forward-delete removes the first rune.
	f.home()
	f.deleteForward()
	if got := f.fields[0].value; got != "aXin" {
		t.Errorf("delete at home = %q, want %q", got, "aXin")
	}

	// Right past the end clamps; End jumps to the end.
	f.end()
	if f.fields[0].cursor != len([]rune("aXin")) {
		t.Errorf("end: cursor = %d, want %d", f.fields[0].cursor, len([]rune("aXin")))
	}
	f.right() // no-op at end
	if f.fields[0].cursor != len([]rune("aXin")) {
		t.Errorf("right at end should clamp, got %d", f.fields[0].cursor)
	}
}

// Switching fields moves the caret to the end of the newly focused field.
func TestFormFocusResetsCursorToEnd(t *testing.T) {
	f := &formModel{fields: []formField{{value: "abc"}, {value: "hello"}}}
	f.focusEnd()
	f.next()
	if f.focus != 1 || f.fields[1].cursor != 5 {
		t.Errorf("next: focus=%d cursor=%d, want 1/5", f.focus, f.fields[1].cursor)
	}
	f.prev()
	if f.focus != 0 || f.fields[0].cursor != 3 {
		t.Errorf("prev: focus=%d cursor=%d, want 0/3", f.focus, f.fields[0].cursor)
	}
}
