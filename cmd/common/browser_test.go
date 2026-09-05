package common

import "testing"

func TestOpenBrowserRejectsNonLoopbackURL(t *testing.T) {
	if err := OpenBrowser("https://example.com"); err == nil {
		t.Fatal("OpenBrowser accepted a non-loopback url")
	}
	if err := OpenBrowser("javascript:alert(1)"); err == nil {
		t.Fatal("OpenBrowser accepted a javascript: url")
	}
}
