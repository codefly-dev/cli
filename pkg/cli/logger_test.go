package cli

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// NOTE: these tests mutate the package-level maxUnique global to
// pin the padding width. Do NOT call t.Parallel() in this file —
// concurrent assignment to maxUnique races and produces flaky
// width assertions. If you need parallelism here, refactor
// formattedLogLines to take width as a parameter instead.

func TestFormattedLogLinesTrimsForwardNewlines(t *testing.T) {
	oldMax := maxUnique
	maxUnique = len("infra/postgres")
	defer func() { maxUnique = oldMax }()

	lines := formattedLogLines("infra/postgres", ">", &wool.Log{
		Level:   wool.FORWARD,
		Message: "database ready\n\n",
	})

	if len(lines) != 1 {
		t.Fatalf("expected one rendered line, got %d: %#v", len(lines), lines)
	}
	if lines[0] != "infra/postgres > database ready" {
		t.Fatalf("unexpected rendered line: %q", lines[0])
	}
}

func TestFormattedLogLinesPrefixesEachMultilineEntry(t *testing.T) {
	oldMax := maxUnique
	maxUnique = len("infra/postgres")
	defer func() { maxUnique = oldMax }()

	lines := formattedLogLines("infra/postgres", ">", &wool.Log{
		Level:   wool.FORWARD,
		Message: "first\nsecond\n",
	})

	expected := []string{
		"infra/postgres > first",
		"infra/postgres > second",
	}
	if len(lines) != len(expected) {
		t.Fatalf("expected %d lines, got %d: %#v", len(expected), len(lines), lines)
	}
	for i := range expected {
		if lines[i] != expected[i] {
			t.Fatalf("line %d: expected %q, got %q", i, expected[i], lines[i])
		}
	}
}

// TestWithSilence_ConcurrentLog_RaceFree regression-covers the
// silentMu lock added to the package-level silent slice. Before the
// fix, WithSilence appended without synchronization while Log read
// the slice — race detector fired immediately under concurrent
// access. The test reproduces that exact pattern: callers logging
// while WithSilence is invoked from another goroutine.
func TestWithSilence_ConcurrentLog_RaceFree(t *testing.T) {
	stop := make(chan struct{})
	done := make(chan struct{})

	// Caller: log forward-level entries that exercise the silent-slice read.
	go func() {
		defer close(done)
		ident := &wool.Identifier{Unique: "infra/postgres"}
		entry := &wool.Log{Level: wool.FORWARD, Message: "tick"}
		for {
			select {
			case <-stop:
				return
			default:
				Log(ident, entry)
			}
		}
	}()

	// Mutator: append to the silent slice. Race detector watches the
	// concurrent write/read on the package-level slice.
	svcs := []*resources.ServiceWithModule{
		{Name: "redis", Module: "infra"},
		{Name: "postgres", Module: "infra"},
	}
	for i := 0; i < 200; i++ {
		WithSilence(svcs)
	}

	close(stop)
	<-done
}
