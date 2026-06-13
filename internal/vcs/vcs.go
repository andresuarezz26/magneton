// Package vcs opens PRs via `gh` and renders the PR body + Jira comment from
// user-editable templates (Decision 10).
package vcs

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/andresuarezz26/magneton/internal/paths"
)

// PlanData is the template context for the plan Jira comment.
type PlanData struct {
	Ticket     string
	Summary    string
	Plan       string
	Steps      []string
	Questions  []string
	Confidence string
	Type       string
}

// PRData is the template context for both the PR body and the Jira comment.
type PRData struct {
	Ticket       string
	Summary      string
	Branch       string
	Base         string
	FilesChanged []string
	Tests        string
	Attempts     int
	PRURL        string
	JiraBaseURL  string
}

const defaultPRBody = `# [{{.Ticket}}] {{.Summary}}

Ticket: {{.Ticket}} · Branch: {{.Branch}}

## Summary
{{.Summary}}

## Changes
{{range .FilesChanged}}- {{.}}
{{end}}
## Checks
{{.Tests}}

🤖 Generated autonomously by magneton · review before merge
`

const defaultJiraComment = `✅ PR ready for review → {{.PRURL}}
{{.Tests}} · {{.Attempts}} attempt(s)
`

const defaultPlanComment = `🤖 magneton plan for [{{.Ticket}}] {{.Summary}}

*Approach:* {{.Plan}}
*Type:* {{.Type}} · *Confidence:* {{.Confidence}}

*Steps:*
{{range .Steps}}- {{.}}
{{end}}
{{- if .Questions}}
*Questions before I start — please reply to this comment:*
{{range .Questions}}- {{.}}
{{end}}
{{- else}}
No blockers — proceeding with implementation automatically.
{{- end}}`

func render(name, def string, data any) (string, error) {
	text := def
	if b, err := os.ReadFile(filepath.Join(paths.Templates(), name)); err == nil {
		text = string(b)
	}
	tmpl, err := template.New(name).Parse(text)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// RenderPRBody renders the PR description.
func RenderPRBody(d PRData) (string, error) { return render("pr_body.tmpl", defaultPRBody, d) }

// RenderJiraComment renders the ticket comment.
func RenderJiraComment(d PRData) (string, error) {
	return render("jira_comment.tmpl", defaultJiraComment, d)
}

// RenderPlanComment renders the plan stage Jira comment.
func RenderPlanComment(d PlanData) (string, error) {
	return render("plan_comment.tmpl", defaultPlanComment, d)
}

// OpenPR creates a PR with gh and returns its URL.
func OpenPR(worktreeDir, base, title, body string) (string, error) {
	tmp, err := os.CreateTemp("", "magneton-pr-*.md")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(body); err != nil {
		return "", err
	}
	tmp.Close()

	cmd := exec.Command("gh", "pr", "create", "--base", base, "--title", title, "--body-file", tmp.Name())
	cmd.Dir = worktreeDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr create: %w\n%s", err, out)
	}
	for _, f := range strings.Fields(string(out)) {
		if strings.HasPrefix(f, "http") {
			return f, nil
		}
	}
	return strings.TrimSpace(string(out)), nil
}

// PRState returns the PR's state via gh: "OPEN", "MERGED", or "CLOSED".
// repoDir gives gh the repository context.
func PRState(repoDir, prURL string) (string, error) {
	cmd := exec.Command("gh", "pr", "view", prURL, "--json", "state", "-q", ".state")
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr view: %w\n%s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// WriteDefaultTemplates drops the default templates if they don't exist yet.
func WriteDefaultTemplates() error {
	defaults := map[string]string{
		"pr_body.tmpl":      defaultPRBody,
		"jira_comment.tmpl": defaultJiraComment,
		"plan_comment.tmpl": defaultPlanComment,
	}
	for name, def := range defaults {
		p := filepath.Join(paths.Templates(), name)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if err := os.WriteFile(p, []byte(def), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}
