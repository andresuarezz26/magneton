// Package notify sends a desktop notification plus a daemon-log line when a
// session needs a human or finishes (Decision 11).
package notify

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/andresuarezz26/magneton/internal/paths"
)

// Send fires a native OS notification (best effort) and appends to daemon.log.
func Send(title, body string) {
	logLine(fmt.Sprintf("%s - %s", title, body))
	if runtime.GOOS == "darwin" {
		script := fmt.Sprintf("display notification %q with title %q", body, title)
		_ = exec.Command("osascript", "-e", script).Run()
	}
	// Linux/Windows notifiers can be added here later.
}

func logLine(msg string) {
	f, err := os.OpenFile(paths.DaemonLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  %s\n", time.Now().Format(time.RFC3339), strings.TrimSpace(msg))
}
