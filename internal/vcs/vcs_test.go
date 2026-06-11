package vcs

import (
	"strings"
	"testing"
)

func TestRenderPRBody(t *testing.T) {
	out, err := RenderPRBody(PRData{
		Ticket:       "PROJ-388",
		Summary:      "Remove deprecated lint baseline entries",
		Branch:       "ai/proj-388-lint",
		FilesChanged: []string{"app/lint-baseline.xml"},
		Tests:        "✅ compile + unit tests",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[PROJ-388]", "ai/proj-388-lint", "app/lint-baseline.xml", "compile + unit"} {
		if !strings.Contains(out, want) {
			t.Errorf("PR body missing %q\n%s", want, out)
		}
	}
}

func TestRenderJiraComment(t *testing.T) {
	out, err := RenderJiraComment(PRData{PRURL: "https://gh/x/pull/1", Tests: "pass", Attempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pull/1") || !strings.Contains(out, "3 attempt") {
		t.Errorf("unexpected comment: %s", out)
	}
}
