package common

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// OpenBrowser opens url in the user's default browser. Failure is not fatal:
// it returns an error the caller reports as a warning. Never used in headless CI.
func OpenBrowser(url string) error {
	if !strings.HasPrefix(url, "http://127.0.0.1:") && !strings.HasPrefix(url, "http://localhost:") {
		return fmt.Errorf("refusing to open non-loopback url: %q", url)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
