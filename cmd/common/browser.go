package common

import (
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
)

// OpenBrowser opens target in the user's default browser. Failure is not
// fatal: it returns an error the caller reports as a warning. Never used in
// headless CI.
func OpenBrowser(target string) error {
	if err := validateLoopbackURL(target); err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}

// validateLoopbackURL rejects anything but a plain http URL to 127.0.0.1 or
// localhost, judged by parsed URL authority rather than by string prefix. A
// prefix check alone is fooled by an embedded-credentials URL like
// "http://127.0.0.1:1234@evil.com/": it starts with the expected text, but
// per RFC 3986 (and every mainstream browser) "127.0.0.1:1234" is userinfo
// and "evil.com" is the actual host that gets navigated to.
func validateLoopbackURL(target string) error {
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" {
		return fmt.Errorf("refusing to open non-http url: %q", target)
	}
	if u.User != nil {
		return fmt.Errorf("refusing to open url with embedded credentials: %q", target)
	}
	if host := u.Hostname(); host != "127.0.0.1" && host != "localhost" {
		return fmt.Errorf("refusing to open non-loopback url: %q", target)
	}
	return nil
}
