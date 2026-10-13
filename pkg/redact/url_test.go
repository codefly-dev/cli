package redact

import (
	"strings"
	"testing"
)

const secret = "gho_notarealtoken"

func TestRemoteMasksEveryShapeOfUserinfo(t *testing.T) {
	for _, tc := range []struct{ remote, want string }{
		{"https://x-access-token:" + secret + "@github.com/codefly-dev/cli.git\n", "https://***@github.com/codefly-dev/cli.git"},
		{"https://" + secret + "@github.com/codefly-dev/cli", "https://***@github.com/codefly-dev/cli"},
		{"https://" + secret + ":x-oauth-basic@github.com/codefly-dev/cli", "https://***@github.com/codefly-dev/cli"},
		{"https://user:pa/ss" + secret + "@gitlab.com/o/r", "https://***@gitlab.com/o/r"},
		{"ssh://git:" + secret + "@github.com/o/r", "ssh://***@github.com/o/r"},
		{secret + "@gitlab.com:o/r", "***@gitlab.com:o/r"},
		{"git@gitlab.com:o/r", "git@gitlab.com:o/r"},
		{"https://github.com/o/r", "https://github.com/o/r"},
		{"/srv/git/r.git", "/srv/git/r.git"},
	} {
		t.Run(tc.remote, func(t *testing.T) {
			got := Remote(tc.remote)
			if strings.Contains(got, secret) {
				t.Fatalf("Remote(%q) leaks the credential: %q", tc.remote, got)
			}
			if got != tc.want {
				t.Fatalf("Remote(%q) = %q, want %q", tc.remote, got, tc.want)
			}
		})
	}
}

func TestURLCredentialsMasksEveryURLInText(t *testing.T) {
	text := "fatal: could not read from https://x-access-token:" + secret + "@github.com/o/lib.git/\n" +
		"retrying ssh://" + secret + "@github.com/o/other and https://proxy.golang.org/github.com/o/lib/@v/list"
	got := URLCredentials(text)
	if strings.Contains(got, secret) {
		t.Fatalf("URLCredentials leaks the credential: %q", got)
	}
	for _, want := range []string{
		"https://***@github.com/o/lib.git/",
		"ssh://***@github.com/o/other",
		"https://proxy.golang.org/github.com/o/lib/@v/list",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("URLCredentials(%q) = %q, want it to contain %q", text, got, want)
		}
	}
}
