package build

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// SDKPaths holds resolved absolute paths to Android SDK binaries.
// Use ResolvePaths to build one from the sdk root directory.
type SDKPaths struct {
	ADB      string // e.g. /Users/.../sdk/platform-tools/adb
	Emulator string // e.g. /Users/.../sdk/emulator/emulator
}

// ResolvePaths resolves adb and emulator from sdkRoot, falling back to PATH
// if the SDK-relative paths don't exist.
func ResolvePaths(sdkRoot string) SDKPaths {
	resolve := func(rel, name string) string {
		if sdkRoot != "" {
			p := filepath.Join(sdkRoot, rel)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
		return name // last resort: let exec fail with a clear message
	}
	return SDKPaths{
		ADB:      resolve(filepath.Join("platform-tools", "adb"), "adb"),
		Emulator: resolve(filepath.Join("emulator", "emulator"), "emulator"),
	}
}

// AlreadyRunning reports whether any Android emulator is currently connected.
func AlreadyRunning(p SDKPaths) bool {
	out, err := exec.Command(p.ADB, "devices").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.HasPrefix(fields[0], "emulator-") && fields[1] == "device" {
			return true
		}
	}
	return false
}

// Start launches an AVD in the background with no window, no audio, and no
// snapshot load. Returns the OS pid of the emulator process.
func Start(p SDKPaths, avdName string) (int, error) {
	cmd := exec.Command(p.Emulator,
		"-avd", avdName,
		"-no-window",
		"-no-audio",
		"-no-snapshot-load",
	)
	cmd.Stdout = nil // suppress verbose emulator INFO logs
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("emulator start %q: %w", avdName, err)
	}
	return cmd.Process.Pid, nil
}

// WaitReady polls `adb shell getprop sys.boot_completed` every 3 seconds until
// the emulator is fully booted or ctx is cancelled/times out.
func WaitReady(ctx context.Context, p SDKPaths) error {
	for {
		out, err := exec.CommandContext(ctx, p.ADB, "shell", "getprop", "sys.boot_completed").Output()
		if err == nil && strings.TrimSpace(string(out)) == "1" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("emulator not ready: %w", ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
}

// Kill sends SIGTERM then SIGKILL to the emulator process. Used on daemon
// shutdown and idle-timeout — NOT after individual test runs.
func Kill(pid int) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	time.Sleep(2 * time.Second)
	_ = proc.Signal(syscall.SIGKILL)
}
