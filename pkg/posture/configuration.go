package posture

import (
	"fmt"
	"sort"
	"strings"
)

// This file reads a workload's rendered configuration. It is NOT the authority on
// what a workload does — the service's declaration is (contracts.go). It exists so
// that a declaration can be CHECKED against the configuration its own agent
// emitted: a service declaring the mesh carries its transport, beside a rendered
// setting that configures TLS, is a contradiction, and a contradiction refuses
// naming both sides.
//
// That is why an unrecognised spelling cannot defeat these rules. A spelling this
// file misses is not a bypass: the declaration still governs, and the runtime that
// owns the behaviour is still bound by it. What this file adds is that a
// declaration cannot be contradicted by the very manifest that carries it.

// valueNone is the value that means a setting selects nothing at all.
const valueNone = "none"

// setting is one piece of a container's effective configuration, with the manifest
// field a refusal names.
type setting struct {
	name  string
	value string
	// resolved is false when the value lives outside the manifest set — a Secret
	// key, a field reference — so a rule can fail closed on a name that matters
	// rather than read absence as conformance.
	resolved  bool
	reference string
	field     string
	// flag is true for a command-line option: a bare flag is on.
	flag bool
	// display is the setting as written, so a refusal quotes what its author typed.
	display string
}

// enabled reports whether a setting turns its subject on. An unresolved value is
// treated as on: the render cannot see it, and reading absence as off would make
// the rule optional for anyone willing to move the value.
func (s *setting) enabled() bool {
	if !s.resolved {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(s.value)) {
	case "":
		return s.flag
	case "false", "0", "no", "off", "disabled", valueNone:
		return false
	default:
		return true
	}
}

func (s *setting) describe() string {
	switch {
	case s.display != "":
		return s.display
	case !s.resolved:
		return fmt.Sprintf("%s (from %s, a value this render cannot read)", s.name, s.reference)
	case s.value == "":
		return s.name
	default:
		return fmt.Sprintf("%s=%s", s.name, s.value)
	}
}

// containerConfiguration is one container's effective configuration: every option
// and environment entry, from every place one can arrive.
type containerConfiguration struct {
	container string
	field     string
	settings  []setting
}

// configurations reads every container of a pod specification — ordinary, init and
// ephemeral, because all three run in the cell and any of them can configure the
// workload.
func configurations(spec pathedSpec, index *referenceIndex, namespace string) []containerConfiguration {
	all := containers(spec)
	decoded := make([]containerConfiguration, 0, len(all))
	for _, container := range all {
		configuration := containerConfiguration{
			container: stringAt(container.value, "name"),
			field:     container.path,
		}
		configuration.settings = append(configuration.settings, container.environmentSettings(index, namespace)...)
		configuration.settings = append(configuration.settings, container.argumentSettings()...)
		decoded = append(decoded, configuration)
	}
	return decoded
}

// environmentSettings reads env and envFrom with Kubernetes' own semantics:
// envFrom sources contribute in order, a later source overrides an earlier one, an
// explicit env entry overrides them all, and a source's prefix is applied to every
// key it contributes. Anything else would check a configuration the container does
// not actually receive.
func (container pathedContainer) environmentSettings(index *referenceIndex, namespace string) []setting {
	ordered := map[string]setting{}
	var order []string
	record := func(entry setting) {
		if _, seen := ordered[entry.name]; !seen {
			order = append(order, entry.name)
		}
		ordered[entry.name] = entry
	}
	for position, source := range container.environmentSources() {
		field := fmt.Sprintf("%s.envFrom[%d]", container.path, position)
		entries, resolved := index.sourceEntries(source, namespace)
		if !resolved {
			record(setting{
				name: source.prefix + source.name, resolved: false,
				reference: source.describe(), field: field,
			})
			continue
		}
		for _, key := range sortedKeys(entries) {
			record(setting{
				name: source.prefix + key, value: entries[key], resolved: true,
				field: fmt.Sprintf("%s (%s)", field, source.describe()),
			})
		}
	}
	for position, entry := range container.environment() {
		value, resolved, reference := resolveEnvironmentValue(entry, index, namespace)
		record(setting{
			name: entry.name, value: value, resolved: resolved, reference: reference,
			field: fmt.Sprintf("%s.env[%d]", container.path, position),
		})
	}
	settings := make([]setting, 0, len(order))
	for _, name := range order {
		settings = append(settings, ordered[name])
	}
	return settings
}

// argumentSettings parses a container's command and args into options, in both
// forms an option takes (`--name=value` and `--name value`), and reads through a
// shell invocation: a program carried in one argument is still this container's
// configuration. Comments are stripped first, so an option named in a comment is
// not read as configuration.
func (container pathedContainer) argumentSettings() []setting {
	tokens := container.arguments()
	plain := make([]string, 0, len(tokens))
	paths := make([]string, 0, len(tokens))
	for _, token := range tokens {
		plain = append(plain, token.value)
		paths = append(paths, token.path)
	}
	settings := optionSettings(plain, paths)
	for position, token := range tokens {
		body, isShell := shellBody(tokens, position)
		if !isShell {
			continue
		}
		inner := shellTokens(body)
		innerPaths := make([]string, len(inner))
		for index := range innerPaths {
			innerPaths[index] = token.path + " (shell program)"
		}
		settings = append(settings, optionSettings(inner, innerPaths)...)
	}
	return settings
}

// shellInterpreters are the programs whose -c argument is a script.
var shellInterpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ash": true, "ksh": true, "env": true,
}

// shellBody reports the script a token carries when it is the argument of a shell
// interpreter's -c. Every spelling of that flag counts: `-c`, `-ec`, `-euxc`,
// `--command`. Recognising only the bare `-c` is how a wrapper bypassed this
// check, so the test is "a flag ending in c", not an exact token.
func shellBody(tokens []argumentToken, position int) (string, bool) {
	if position == 0 {
		return "", false
	}
	previous := strings.TrimSpace(tokens[position-1].value)
	if !shellCommandFlag(previous) {
		return "", false
	}
	for before := position - 2; before >= 0; before-- {
		candidate := strings.TrimSpace(tokens[before].value)
		if candidate == "--" || candidate == "" {
			continue
		}
		if shellInterpreters[lastPathSegment(candidate)] {
			return tokens[position].value, true
		}
		break
	}
	return "", false
}

// shellCommandFlag reports whether a token is a shell's "read the next argument as
// a program" flag, in any of the spellings a wrapper uses.
func shellCommandFlag(token string) bool {
	if token == "--command" {
		return true
	}
	if !strings.HasPrefix(token, "-") || strings.HasPrefix(token, "--") {
		return false
	}
	letters := strings.TrimPrefix(token, "-")
	return letters != "" && strings.HasSuffix(letters, "c")
}

func lastPathSegment(value string) string {
	if index := strings.LastIndex(value, "/"); index >= 0 {
		return value[index+1:]
	}
	return value
}

// shellTokens splits a shell body into tokens, dropping comments and quoting. It
// is deliberately simple: it exists so an option written inside a script is read as
// an option, not so the script is understood.
func shellTokens(body string) []string {
	var tokens []string
	for _, line := range strings.Split(body, "\n") {
		if cut := commentIndex(line); cut >= 0 {
			line = line[:cut]
		}
		for _, field := range strings.FieldsFunc(line, func(symbol rune) bool {
			switch symbol {
			case ' ', '\t', ';', '&', '|', '(', ')', '\r':
				return true
			}
			return false
		}) {
			tokens = append(tokens, strings.Trim(field, `"'`))
		}
	}
	return tokens
}

// commentIndex finds where a shell comment starts, ignoring a quoted #.
func commentIndex(line string) int {
	inSingle, inDouble := false, false
	for index, symbol := range line {
		switch symbol {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if inSingle || inDouble {
				continue
			}
			if index == 0 || line[index-1] == ' ' || line[index-1] == '\t' {
				return index
			}
		}
	}
	return -1
}

// optionSettings parses command-line tokens into settings, reading both the joined
// and the split form of an option because a rule that understood only one of them
// would be satisfied by writing the other.
func optionSettings(tokens, paths []string) []setting {
	var settings []setting
	for index, token := range tokens {
		if !strings.HasPrefix(token, "-") || token == "-" || token == "--" {
			continue
		}
		name := strings.TrimLeft(token, "-")
		value, hasValue := "", false
		if split := strings.SplitN(name, "=", 2); len(split) == 2 {
			name, value, hasValue = split[0], split[1], true
		} else if index+1 < len(tokens) && !strings.HasPrefix(tokens[index+1], "-") {
			value, hasValue = tokens[index+1], true
		}
		if name == "" {
			continue
		}
		settings = append(settings, setting{
			name: name, value: strings.Trim(value, `"'`), resolved: true,
			field: paths[index], flag: !hasValue, display: token,
		})
	}
	return settings
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
