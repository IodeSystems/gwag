package main

import (
	"fmt"
	"os"
)

// stderrIsTTY reports whether stderr is a terminal.
func stderrIsTTY() bool {
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// underService reports whether the process looks service-managed
// (systemd, launchd).
func underService() bool {
	for _, k := range []string{"NOTIFY_SOCKET", "INVOCATION_ID", "XPC_SERVICE_NAME"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// tokenLine renders one startup log line for a token. The value is
// printed only when tty; otherwise logs (journal, docker, CI) get the
// file path and a hint. path is "" for an in-memory token.
func tokenLine(label, value, path string, tty, service bool) string {
	switch {
	case tty && path != "":
		return fmt.Sprintf("%s = %s  (persisted to %s)", label, value, path)
	case tty:
		return fmt.Sprintf("%s = %s  (in-memory)", label, value)
	case path != "" && service:
		return fmt.Sprintf("%s = (not logged; stored in %s; run 'gwag service login')", label, path)
	case path != "":
		return fmt.Sprintf("%s = (not logged; stored in %s; read the file)", label, path)
	default:
		return fmt.Sprintf("%s = (not logged; in-memory, not stored; run with a terminal on stderr or set --admin-data-dir)", label)
	}
}
