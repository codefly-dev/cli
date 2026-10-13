package publish

import (
	"bytes"
	"fmt"
	"strings"
)

// replaceVersionLine rewrites the top-level `version: <X>` line in the
// raw YAML file to `version: <newVersion>`, leaving every other byte
// (comments, blank lines, sibling fields, indentation) untouched.
//
// Only an unindented line is the top-level key — the one readVersion reads. A
// service manifest also carries `version:` under its agent and under each
// library dependency, and either may come first in the file; rewriting the
// first `version:` found would retarget the service's agent instead of
// releasing the service.
//
// We do NOT round-trip through a YAML parser — gopkg.in/yaml.v3 emits
// canonical output that strips comments and may reorder/reflow
// fields. The release flow needs minimum-diff edits so reviewers can
// see "this is just a version bump" at a glance.
//
// Match shape: `version:` at the start of a line + optional
// whitespace + the value + optional trailing comment. Trailing
// comment is preserved (the version's value is replaced; the comment
// stays).
func replaceVersionLine(raw []byte, newVersion string) ([]byte, error) {
	lines := bytes.Split(raw, []byte("\n"))

	for i, line := range lines {
		// Match the top-level key only: `version:` at the start of the line.
		if !bytes.HasPrefix(line, []byte("version:")) {
			continue
		}
		afterColon := line[len("version:"):]

		// Preserve a trailing comment if present.
		// Split on " #" because YAML comments require a space before
		// `#` when they follow a value (per spec); not on bare `#`.
		var tail []byte
		if idx := bytes.Index(afterColon, []byte(" #")); idx >= 0 {
			tail = afterColon[idx:] // includes the leading " #"
		}

		newLine := []byte("version: ")
		newLine = append(newLine, []byte(newVersion)...)
		newLine = append(newLine, tail...)
		lines[i] = newLine
		return bytes.Join(lines, []byte("\n")), nil
	}
	return nil, fmt.Errorf("no top-level `version:` line found in manifest")
}

// quoteIfNeeded wraps newVersion in YAML quotes when it contains
// characters YAML's plain-scalar style would reinterpret. Reserved
// for future use — semver-only versions never need quoting.
func quoteIfNeeded(v string) string {
	for _, special := range []string{":", "#", ",", "[", "]", "{", "}"} {
		if strings.Contains(v, special) {
			return fmt.Sprintf("%q", v)
		}
	}
	return v
}

// silence unused warning on quoteIfNeeded — kept for future scenarios
// where the version string is exotic.
var _ = quoteIfNeeded
