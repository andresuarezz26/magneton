package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/andresuarezz26/magneton/internal/config"
	"github.com/andresuarezz26/magneton/internal/jira"
	"github.com/andresuarezz26/magneton/internal/paths"
	"github.com/andresuarezz26/magneton/internal/secrets"
)

// resolveSDKBinary finds a binary by name, first in PATH, then under the SDK
// directory at known subdirectory locations. Returns the resolved path or "".
func resolveSDKBinary(name, sdkPath string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if sdkPath == "" {
		return ""
	}
	sdkPath = config.Expand(sdkPath)
	candidates := map[string]string{
		"adb":      filepath.Join(sdkPath, "platform-tools", "adb"),
		"emulator": filepath.Join(sdkPath, "emulator", "emulator"),
	}
	if p, ok := candidates[name]; ok {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func checkSDKBinary(name, sdkPath, hint string) error {
	if p := resolveSDKBinary(name, sdkPath); p != "" {
		return nil
	}
	return fmt.Errorf("not found in PATH or %s - %s", sdkPath, hint)
}

func checkAVD(avdName, sdkPath string) error {
	emuBin := resolveSDKBinary("emulator", sdkPath)
	if emuBin == "" {
		return errors.New("emulator binary not found - cannot verify AVD")
	}
	out, err := exec.Command(emuBin, "-list-avds").Output()
	if err != nil {
		return fmt.Errorf("emulator -list-avds failed: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == avdName {
			return nil
		}
	}
	return fmt.Errorf("AVD %q not found - run: avdmanager list avd", avdName)
}

func checkGH() error {
	if _, err := exec.LookPath("gh"); err != nil {
		return errors.New("not found in PATH")
	}
	if err := exec.Command("gh", "auth", "status").Run(); err != nil {
		return errors.New("installed but not authenticated - run: gh auth login")
	}
	return nil
}

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "doctor",
		Short: "Run the connectivity check against the saved config (no prompts)",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			fmt.Printf("config: %s\n\n", paths.Config())
			fmt.Println("connectivity check")
			jc := jira.New(cfg.JiraBaseURL, cfg.JiraEmail, secrets.Get(secrets.Jira))
			report("Jira", jc.Verify())
			for _, r := range cfg.Repos {
				report("git remote (origin) - "+r.Path, checkGitRemote(config.Expand(r.Path)))
			}
			report("claude CLI", exec.Command("claude", "--version").Run())
			report("gh CLI", checkGH())

			fmt.Println("\nemulator (optional - used automatically for UI/instrumentation tasks)")
			report("adb", checkSDKBinary("adb", cfg.AndroidSDKPath, "install Android SDK platform-tools"))
			report("emulator", checkSDKBinary("emulator", cfg.AndroidSDKPath, "install Android SDK emulator package"))
			if cfg.AVDName != "" {
				report("AVD "+cfg.AVDName, checkAVD(cfg.AVDName, cfg.AndroidSDKPath))
			} else {
				fmt.Println("  ℹ avd_name not set in config - tasks needing emulator will fall back to unit tests")
			}
			return nil
		},
	})
}
