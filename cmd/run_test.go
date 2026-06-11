package cmd

import "testing"

func TestNormalizeTicket(t *testing.T) {
	cases := map[string]string{
		"proj-1":    "PROJ-1",
		"  abc-2  ": "ABC-2",
		"X-9":       "X-9",
	}
	for in, want := range cases {
		if got := normalizeTicket(in); got != want {
			t.Errorf("normalizeTicket(%q) = %q, want %q", in, got, want)
		}
	}
}
