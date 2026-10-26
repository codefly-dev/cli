// Package redact keeps credentials that ride inside git remote URLs out of
// anything the CLI prints, logs or returns as an error.
//
// A url.<base>.insteadOf rewrite (and a CI checkout) makes git resolve a remote
// to a URL that carries a live access token in its userinfo:
// https://x-access-token:<token>@github.com/owner/repo. Every string that
// came out of git and is about to be shown to someone goes through here first.
package redact

import (
	"regexp"
	"strings"
)

// Mask stands in for redacted userinfo.
const Mask = "***"

// urlUserinfo matches the userinfo of every URL embedded in free text. It stops
// at the first '/', whitespace or '@', so a path that contains an '@' (a Go
// module proxy's /@v/ listing) is left readable.
var urlUserinfo = regexp.MustCompile(`://[^/\s@]+@`)

// URLCredentials masks the userinfo of every URL inside text — a command's
// stderr, a joined argument list. Use Remote for a value that is one remote.
func URLCredentials(text string) string {
	return urlUserinfo.ReplaceAllString(text, "://"+Mask+"@")
}

// Remote returns remote, trimmed, with its whole userinfo masked. It is meant
// for one remote that is about to be printed — typically one a parser has just
// refused — so it does not assume the value is well formed.
//
// The whole userinfo goes, not only the half after a colon: GitHub accepts a
// token as the user name (https://<token>@github.com/, and the legacy
// https://<token>:x-oauth-basic@github.com/), so no part of it is safe to keep.
// Everything up to the LAST '@' is userinfo, so an unencoded '/' or ':' in a
// secret is masked rather than taken for the start of the host. An scp-style
// remote (user@host:path) is masked the same way, except for the conventional
// "git" user, which names no one.
func Remote(remote string) string {
	trimmed := strings.TrimSpace(remote)
	start := 0
	if scheme := strings.Index(trimmed, "://"); scheme >= 0 {
		start = scheme + len("://")
	}
	at := strings.LastIndexByte(trimmed[start:], '@')
	if at < 0 {
		return trimmed
	}
	if start == 0 && trimmed[:at] == "git" {
		return trimmed
	}
	return trimmed[:start] + Mask + trimmed[start+at:]
}
