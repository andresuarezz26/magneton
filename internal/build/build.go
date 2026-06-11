// Package build runs the configured Gradle compile + unit-test commands and
// captures pass/fail + output for the self-correct loop (Decisions 3-bar, 4).
package build

import (
	"os"
	"os/exec"
	"strings"
)

// Result is the outcome of a gate run.
type Result struct {
	OK     bool
	Phase  string // "compile" or "test" (empty when OK)
	Output string
}

// Step runs a single gate command (compile or test). An empty command is a pass.
// phase labels the failure (and lets the caller drive lifecycle state).
func Step(dir, gradleHome, command, phase string) Result {
	if strings.TrimSpace(command) == "" {
		return Result{OK: true}
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GRADLE_USER_HOME="+gradleHome)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Result{Phase: phase, Output: string(out)}
	}
	return Result{OK: true}
}
