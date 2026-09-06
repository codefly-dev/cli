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

// TestValidateLoopbackURLRejectsEmbeddedCredentialsBypass is the regression
// test for a naive prefix check: "http://127.0.0.1:1234@evil.com/" starts
// with "http://127.0.0.1:" as a string, but parses (per RFC 3986, and per
// every mainstream browser) to host "evil.com" with "127.0.0.1:1234" as
// discarded userinfo. A prefix match alone would let this through.
func TestValidateLoopbackURLRejectsEmbeddedCredentialsBypass(t *testing.T) {
	if err := validateLoopbackURL("http://127.0.0.1:1234@evil.com/x"); err == nil {
		t.Fatal("validateLoopbackURL accepted a url with a spoofed loopback prefix and a real external host")
	}
}

func TestValidateLoopbackURLAcceptsLoopbackURLs(t *testing.T) {
	for _, target := range []string{
		"http://127.0.0.1:8080/",
		"http://127.0.0.1:8080",
		"http://localhost:3000/dashboard",
	} {
		if err := validateLoopbackURL(target); err != nil {
			t.Errorf("validateLoopbackURL(%q) = %v, want nil", target, err)
		}
	}
}

func TestValidateLoopbackURLRejectsNonHTTPOrNonLoopback(t *testing.T) {
	for _, target := range []string{
		"https://127.0.0.1:8080/",         // https, not the exact scheme codefly serves on
		"http://192.168.1.1:8080/",        // real host, not loopback
		"file:///etc/passwd",              // non-http scheme
		"javascript:alert(1)",             // non-http scheme
		"http://127.0.0.1:1234@evil.com/", // embedded-credentials bypass attempt
		"://not a url",                    // unparsable
	} {
		if err := validateLoopbackURL(target); err == nil {
			t.Errorf("validateLoopbackURL(%q) = nil, want an error", target)
		}
	}
}
