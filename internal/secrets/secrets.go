// Package secrets reads long-lived tokens (Decision 14: OS keychain, env-var fallback).
//
// On macOS it shells out to the `security` CLI so we don't pull in a cgo/keychain
// dependency. Env vars ($MAGNETON_<KEY>_TOKEN) always win, for headless/CI use.
package secrets

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

const service = "magneton"

// Logical secret keys.
const (
	Jira      = "jira"
	Git       = "git"
	Anthropic = "anthropic"
)

func envName(key string) string {
	return "MAGNETON_" + strings.ToUpper(key) + "_TOKEN"
}

// Get returns the secret for key: env var first, then the OS keychain.
func Get(key string) string {
	if v := os.Getenv(envName(key)); v != "" {
		return v
	}
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", key, "-w").Output()
		if err == nil {
			return strings.TrimRight(string(out), "\n")
		}
	}
	return ""
}

// Set stores a secret in the OS keychain (best effort; macOS only for now).
func Set(key, val string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("security", "add-generic-password", "-s", service, "-a", key, "-w", val, "-U").Run()
	}
	return nil
}
