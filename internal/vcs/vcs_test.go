package vcs

import (
	"os"
	"path/filepath"
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

func TestMissingSections(t *testing.T) {
	tmpl := "## Summary\nDescribe the change.\n\n## Changes\n- list files\n\n## Checks\n- [ ] tests\n\n## Notes\nOptional.\n"

	// All sections present → nil.
	full := "## Summary\nDid X.\n\n## Changes\n- foo.kt\n\n## Checks\n- [x] tests\n\n## Notes\nNone.\n"
	if got := MissingSections(tmpl, full); got != nil {
		t.Errorf("full body: expected nil, got %v", got)
	}

	// Two sections absent.
	partial := "## Summary\nDid X.\n\n## Checks\n- [x] tests\n"
	got := MissingSections(tmpl, partial)
	if len(got) != 2 {
		t.Fatalf("partial body: want 2 missing, got %v", got)
	}
	missing := map[string]bool{got[0]: true, got[1]: true}
	if !missing["Changes"] || !missing["Notes"] {
		t.Errorf("wrong missing sections: %v", got)
	}

	// Case-insensitive: "checks" matches "## Checks".
	caseBody := "## summary\nDid X.\n## changes\n- foo\n## checks\n- ok\n## notes\n- none\n"
	if got := MissingSections(tmpl, caseBody); got != nil {
		t.Errorf("case-insensitive: expected nil, got %v", got)
	}
}

func TestRenderPRBodyEmptyFallbacks(t *testing.T) {
	// Use a temp home so the disk template (if any) doesn't shadow the default.
	t.Setenv("MAGNETON_HOME", t.TempDir())

	// Empty FilesChanged → fallback text, not a bare heading.
	out, err := RenderPRBody(PRData{Ticket: "T-1", Summary: "fix", Branch: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(not reported)") {
		t.Errorf("empty FilesChanged: expected fallback text, got:\n%s", out)
	}
	if !strings.Contains(out, "## Changes") {
		t.Errorf("Changes heading missing:\n%s", out)
	}

	// Non-empty Tests field renders normally.
	out2, err := RenderPRBody(PRData{
		Ticket:       "T-2",
		Summary:      "fix",
		Branch:       "b",
		FilesChanged: []string{"a.kt"},
		Tests:        "✅ pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2, "(not reported)") {
		t.Errorf("non-empty fields should not show fallback:\n%s", out2)
	}
	if !strings.Contains(out2, "a.kt") {
		t.Errorf("expected file in output:\n%s", out2)
	}
}

func TestRepairSections(t *testing.T) {
	tmpl := "## Summary\nDescribe.\n\n## Changes\n- list files\n\n## Notes\nOptional.\n"
	body := "## Summary\nDid X.\n"

	missing := MissingSections(tmpl, body)
	if len(missing) != 2 {
		t.Fatalf("pre-repair: want 2 missing, got %v", missing)
	}
	repaired := RepairSections(tmpl, body, missing)

	// Existing content untouched.
	if !strings.Contains(repaired, "## Summary\nDid X.") {
		t.Errorf("original body modified: %q", repaired)
	}
	// Missing sections appended verbatim.
	if !strings.Contains(repaired, "## Changes") || !strings.Contains(repaired, "## Notes") {
		t.Errorf("missing sections not appended:\n%s", repaired)
	}
	// After repair, MissingSections should return nil.
	if got := MissingSections(tmpl, repaired); got != nil {
		t.Errorf("still missing after repair: %v", got)
	}
}

func TestReadRepoTemplate(t *testing.T) {
	dir := t.TempDir()

	// No template → empty string.
	if got := ReadRepoTemplate(dir); got != "" {
		t.Errorf("no template: expected \"\", got %q", got)
	}

	// .github/PULL_REQUEST_TEMPLATE.md → found.
	ghDir := filepath.Join(dir, ".github")
	if err := os.MkdirAll(ghDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := "## Summary\n- list here\n"
	if err := os.WriteFile(filepath.Join(ghDir, "PULL_REQUEST_TEMPLATE.md"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadRepoTemplate(dir); got != want {
		t.Errorf("ReadRepoTemplate = %q, want %q", got, want)
	}
}
