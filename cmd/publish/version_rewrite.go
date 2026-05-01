package publish

import (
	"bytes"
	"fmt"
	"strings"
)

// replaceVersionLine rewrites the first `version: <X>` line in the
// raw YAML file to `version: <newVersion>`, leaving every other byte
// (comments, blank lines, sibling fields, indentation) untouched.
//
// We do NOT round-trip through a YAML parser — gopkg.in/yaml.v3 emits
// canonical output that strips comments and may reorder/reflow
// fields. The release flow needs minimum-diff edits so reviewers can
// see "this is just a version bump" at a glance.
//
// Match shape: optional leading whitespace + `version:` + optional
// whitespace + the value + optional trailing comment. Trailing
// comment is preserved (the version's value is replaced; the comment
// stays).
func replaceVersionLine(raw []byte, newVersion string) ([]byte, error) {
	lines := bytes.Split(raw, []byte("\n"))

	for i, line := range lines {
		// Skip blank lines, comment-only lines.
		trimmed := bytes.TrimLeft(line, " \t")
		if len(trimmed) == 0 || trimmed[0] == '#' {
			continue
		}
		// Match `version:` at start of trimmed text.
		if !bytes.HasPrefix(trimmed, []byte("version:")) {
			continue
		}

		// Reconstruct: <indent>version: <newVersion><tail>
		indentEnd := len(line) - len(trimmed) // number of leading whitespace bytes
		indent := line[:indentEnd]
		afterColon := trimmed[len("version:"):]

		// Preserve a trailing comment if present.
		// Split on " #" because YAML comments require a space before
		// `#` when they follow a value (per spec); not on bare `#`.
		var tail []byte
		if idx := bytes.Index(afterColon, []byte(" #")); idx >= 0 {
			tail = afterColon[idx:] // includes the leading " #"
		}

		newLine := append([]byte{}, indent...)
		newLine = append(newLine, []byte("version: ")...)
		newLine = append(newLine, []byte(newVersion)...)
		newLine = append(newLine, tail...)
		lines[i] = newLine
		return bytes.Join(lines, []byte("\n")), nil
	}
	return nil, fmt.Errorf("no `version:` line found in manifest")
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
