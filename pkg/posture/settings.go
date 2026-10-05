package posture

import (
	"fmt"
	"strings"
)

// setting is one piece of a container's effective configuration: a name, the
// value it resolves to, whether that value could be resolved at all, and the
// manifest field a refusal names.
//
// A rule reads settings rather than raw manifest text, so one spelling of a
// thing is not treated as proof that no other spelling exists: an option on the
// command line, an environment entry, a value arriving through a ConfigMap and
// the same option inside a shell command all reach a rule as the same shape.
type setting struct {
	name string
	// value is the resolved value, empty for a bare flag.
	value string
	// resolved is false when the value lives outside the manifest set (a Secret
	// key, a field reference), so a rule can fail closed on a name that matters
	// instead of reading an absent value as conformance.
	resolved bool
	// reference names where an unresolved value comes from.
	reference string
	field     string
	// flag is true for a command-line option, false for an environment entry.
	flag bool
	// display is how the setting is written in the manifest, so a refusal quotes
	// the option the way its author typed it.
	display string
}

// enabled reports whether a setting turns its subject on. A bare flag is on; a
// value is read for truth, so `--dev=false` and MUTUAL_TLS=false are off, and an
// unresolved value is treated as on — the render cannot see it, and a rule that
// read absence as conformance would be satisfied by hiding the value.
func (s *setting) enabled() bool {
	if !s.resolved {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(s.value)) {
	case "":
		return s.flag
	case "false", "0", "no", "off", "disabled", "none":
		return false
	default:
		return true
	}
}

// words is the setting name as upper-case words, for per-word matching.
func (s *setting) words() map[string]bool { return nameWords(s.name) }

// canonical is the setting name with separators removed, for marker matching.
func (s *setting) canonical() string { return canonical(s.name) }

// containerSettings is the effective configuration of one container: its
// command-line options, its environment, and the program it starts.
type containerSettings struct {
	path string
	name string
	// settings are every option and environment entry, in manifest order.
	settings []setting
	// opaqueProgram is set when the container's entrypoint is a shell body the
	// render cannot fully read — a script, a pipeline, a loop. The tokens it
	// could read are still in settings; this says they are not the whole story.
	opaqueProgram string
	// opaqueProgramField names the manifest field holding that body.
	opaqueProgramField string
}

// containerConfiguration extracts a container's effective configuration,
// resolving every reference the manifest set itself can answer.
func containerConfiguration(container pathedContainer, index *referenceIndex) containerSettings {
	configuration := containerSettings{path: container.path, name: stringAt(container.value, "name")}
	configuration.settings = append(configuration.settings, container.environmentSettings(index)...)
	arguments, opaque, opaqueField := container.argumentSettings()
	configuration.settings = append(configuration.settings, arguments...)
	configuration.opaqueProgram, configuration.opaqueProgramField = opaque, opaqueField
	return configuration
}

// environmentSettings reads env and envFrom, resolving what the manifest set
// holds. A key whose value lives in a Secret or a field reference is reported
// unresolved, by name.
func (container pathedContainer) environmentSettings(index *referenceIndex) []setting {
	var settings []setting
	for index2, entry := range container.environment() {
		value, resolved, reference := resolveEnvironmentValue(entry, index)
		settings = append(settings, setting{
			name: entry.name, value: value, resolved: resolved, reference: reference,
			field: fmt.Sprintf("%s.env[%d]", container.path, index2),
		})
	}
	for index2, source := range container.environmentSources() {
		field := fmt.Sprintf("%s.envFrom[%d]", container.path, index2)
		entries, resolved := index.sourceEntries(source)
		if !resolved {
			settings = append(settings, setting{
				name: source.name, resolved: false,
				reference: source.describe(), field: field,
			})
			continue
		}
		for _, key := range sortedStrings(entries) {
			settings = append(settings, setting{
				name: key, value: entries[key], resolved: true,
				field: fmt.Sprintf("%s (%s)", field, source.describe()),
			})
		}
	}
	return settings
}

// argumentSettings parses a container's command and args into options, reading
// through a shell invocation: `sh -c "exec store server -dev"` carries its
// program in one argument, and the tokens of that program are options like any
// other. Comments are stripped first, so a retired option named in a comment is
// not read as configuration.
//
// It also reports the shell body it read and whether that body is one the render
// can only partly understand.
func (container pathedContainer) argumentSettings() ([]setting, string, string) {
	tokens := container.arguments()
	plain := make([]string, 0, len(tokens))
	paths := make([]string, 0, len(tokens))
	for _, token := range tokens {
		plain = append(plain, token.value)
		paths = append(paths, token.path)
	}
	settings := optionSettingsWithPaths(plain, paths)
	opaque, opaqueField := "", ""
	for position, token := range tokens {
		body, isShell := shellBody(tokens, position)
		if !isShell {
			continue
		}
		if opaque == "" {
			opaque, opaqueField = body, token.path
		}
		settings = append(settings, optionSettings(shellTokens(body), token.path+" (shell program)")...)
	}
	return settings, opaque, opaqueField
}

// shellInterpreters are the programs whose -c argument is a script rather than a
// list of options.
var shellInterpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ash": true, "ksh": true,
	"/bin/sh": true, "/bin/bash": true, "/bin/zsh": true, "/bin/dash": true,
	"/bin/ash": true, "/usr/bin/sh": true, "/usr/bin/bash": true, "/usr/bin/env": true,
}

// shellBody reports the script a token carries when it is the argument of a
// shell interpreter's -c.
func shellBody(tokens []argumentToken, position int) (string, bool) {
	if position == 0 {
		return "", false
	}
	if strings.TrimSpace(tokens[position-1].value) != "-c" {
		return "", false
	}
	for before := position - 2; before >= 0; before-- {
		candidate := strings.TrimSpace(tokens[before].value)
		if candidate == "--" || candidate == "" {
			continue
		}
		if shellInterpreters[candidate] || shellInterpreters[lastPathSegment(candidate)] {
			return tokens[position].value, true
		}
		break
	}
	return "", false
}

func lastPathSegment(value string) string {
	if index := strings.LastIndex(value, "/"); index >= 0 {
		return value[index+1:]
	}
	return value
}

// shellTokens splits a shell body into tokens, dropping comments and quoting.
// It is deliberately simple: it exists so that an option written inside a script
// is read as an option, not so that the script is understood. Whatever it cannot
// resolve stays reported as an opaque program.
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

// commentIndex finds where a shell comment starts on a line, ignoring a # that
// is quoted or part of a word.
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

// optionSettings parses tokens into settings, giving every one the same field.
func optionSettings(tokens []string, field string) []setting {
	paths := make([]string, len(tokens))
	for index := range paths {
		paths[index] = field
	}
	return optionSettingsWithPaths(tokens, paths)
}

// optionSettingsWithPaths parses command-line tokens into settings. It reads
// both forms an option takes — `--name=value` and `--name value` — because a
// rule that only understood one of them would be satisfied by writing the other.
func optionSettingsWithPaths(tokens, paths []string) []setting {
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

func sortedStrings(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sortStrings(keys)
	return keys
}
