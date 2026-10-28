package orchestration

import (
	"fmt"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// outputRecord is one line of the `--output-env` artifact, already serialized.
//
// The string is converted ONCE, here, and carried through validation to the
// write: a value is an `any`, so converting it again at write time could
// produce a different string from the one that was checked — which is the
// whole hole a format check is supposed to close.
type outputRecord struct {
	key  string
	line string
}

// outputRecords converts an emitter's variables into serialized records and
// holds the complete batch to the format the supported reader actually parses.
//
// The reader (sdk-go LoadRuntimeEnvironmentFile) scans lines, strips one
// trailing CR, cuts at the FIRST `=`, and refuses the whole file if any name
// is not `CODEFLY__` plus at least one character of [A-Z0-9_]. So this is
// transport grammar, not a second semantic carrier matcher, and it applies to
// EVERY record in the batch — bound artifact, ordinary value, override,
// identity, fixture, endpoint alike. A value carrying LF is the reason: the
// reader would read its tail as another record, and `NOTE=note\nCODEFLY__…=B`
// replaces a value no key matching could have protected. CR is refused
// because the reader silently eats a trailing one; NUL because a process
// environment cannot carry it.
//
// Diagnostics name a record's index and a generic reason, or a key that is
// already known to be well formed. A malformed key and every value stay out of
// the message: they are the attacker-controlled halves.
func outputRecords(environments []*resources.EnvironmentVariable) ([]outputRecord, error) {
	records := make([]outputRecord, 0, len(environments))
	for index, env := range environments {
		if env == nil {
			return nil, fmt.Errorf("output environment record %d is empty", index)
		}
		if err := validOutputKey(env.Key); err != nil {
			return nil, fmt.Errorf("output environment record %d: %w", index, err)
		}
		// resources.EnvironmentVariable.String is this same conversion; it is
		// spelled out so the retained line and the checked line are one value.
		line := fmt.Sprintf("%s=%v", env.Key, env.Value)
		if err := validOutputValue(line[len(env.Key)+1:]); err != nil {
			return nil, fmt.Errorf("output environment carrier %s: %w", env.Key, err)
		}
		records = append(records, outputRecord{key: env.Key, line: line})
	}
	return records, nil
}

// validOutputKey is the reader's own name grammar, byte for byte.
func validOutputKey(key string) error {
	const prefix = "CODEFLY__"
	if !strings.HasPrefix(key, prefix) || len(key) == len(prefix) {
		return fmt.Errorf("its name is not a CODEFLY__ carrier, so the supported reader refuses the whole artifact")
	}
	for index := 0; index < len(key); index++ {
		character := key[index]
		if (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '_' {
			return fmt.Errorf("its name is not uppercase ASCII, digits and underscores, so the supported reader refuses the whole artifact")
		}
	}
	return nil
}

// validOutputValue refuses the delimiters this format cannot represent. An
// empty value and a value holding one or many `=` are representable: the
// reader cuts at the first separator and keeps the rest verbatim.
func validOutputValue(value string) error {
	if strings.ContainsAny(value, "\n\r\x00") {
		return fmt.Errorf("its value contains a line break or NUL, which this raw output format cannot represent; direct process delivery carries it unchanged")
	}
	return nil
}

// validRetainedOutputRecords refuses an existing artifact this writer would
// otherwise rewrite line by line. A retained line is not authenticated source
// evidence — syntax says nothing about where a well formed old record came
// from — but rewriting a malformed one would launder it into output this run
// is answerable for.
func validRetainedOutputRecords(existing string) error {
	for number, line := range strings.Split(existing, "\n") {
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return fmt.Errorf("the existing output environment holds a record at line %d with no separator", number+1)
		}
		if err := validOutputKey(key); err != nil {
			return fmt.Errorf("the existing output environment holds a record at line %d whose name is not a supported carrier", number+1)
		}
		// The value too, not just the name: the rewrite reproduces this line
		// verbatim, and a retained value carrying CR or NUL is exactly as
		// unreadable as a new one. Splitting on LF cannot leave an embedded LF
		// here, but CR and NUL survive it.
		if err := validOutputValue(value); err != nil {
			return fmt.Errorf("the existing output environment holds a record at line %d whose value this format cannot represent", number+1)
		}
	}
	return nil
}
