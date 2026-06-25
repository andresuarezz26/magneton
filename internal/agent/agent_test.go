package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// writeAgentJSON writes <dir>/.agent/<name> with the given JSON body.
func writeAgentJSON(t *testing.T, dir, name, body string) {
	t.Helper()
	ad := filepath.Join(dir, ".agent")
	if err := os.MkdirAll(ad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ad, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadPlanAtRoot(t *testing.T) {
	wt := t.TempDir()
	writeAgentJSON(t, wt, "plan.json", `{"plan":"do it","type":"Feature","confidence":"High"}`)
	plan, err := ReadPlan(wt)
	if err != nil {
		t.Fatalf("ReadPlan: %v", err)
	}
	if plan.Plan != "do it" {
		t.Errorf("plan = %q, want %q", plan.Plan, "do it")
	}
}

// When the configured repo is a module inside a larger repo, `git worktree add`
// checks out the whole repo and the agent writes .agent/ into the module subdir.
// ReadPlan must still find it.
func TestReadPlanInSubdir(t *testing.T) {
	wt := t.TempDir()
	module := filepath.Join(wt, "android")
	writeAgentJSON(t, module, "plan.json", `{"plan":"from subdir","type":"Feature","confidence":"High"}`)
	plan, err := ReadPlan(wt)
	if err != nil {
		t.Fatalf("ReadPlan (subdir): %v", err)
	}
	if plan.Plan != "from subdir" {
		t.Errorf("plan = %q, want %q", plan.Plan, "from subdir")
	}
}

// A root-level file wins over a deeper one (the contract location is preferred).
func TestReadPlanRootWinsOverSubdir(t *testing.T) {
	wt := t.TempDir()
	writeAgentJSON(t, wt, "plan.json", `{"plan":"root","type":"Feature","confidence":"High"}`)
	writeAgentJSON(t, filepath.Join(wt, "android"), "plan.json", `{"plan":"sub","type":"Feature","confidence":"High"}`)
	plan, err := ReadPlan(wt)
	if err != nil {
		t.Fatalf("ReadPlan: %v", err)
	}
	if plan.Plan != "root" {
		t.Errorf("plan = %q, want root-level %q", plan.Plan, "root")
	}
}

func TestReadPlanMissing(t *testing.T) {
	if _, err := ReadPlan(t.TempDir()); err == nil {
		t.Error("expected error when plan.json is absent")
	}
}
