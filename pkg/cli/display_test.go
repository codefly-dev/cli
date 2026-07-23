// display_test.go — coverage for the error-chain renderer.
//
// printErrorChain itself writes to stdout via the TUI; we exercise
// the pure helper `unwrapErrorLayers` it delegates to, which is the
// part that has logic worth testing (the rendering loop is one
// strings.Repeat + fmt.Println per line).

package cli

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/core/wool"
	"github.com/fatih/color"
)

// stripANSI removes SGR escape sequences so a test can assert on the plain
// text golor/lipgloss wrapped around it.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\x1b' {
			if j := strings.IndexByte(s[i:], 'm'); j >= 0 {
				i += j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestViewPreservesGolorControlRunesInMessage(t *testing.T) {
	// A message's own '#', '[' and ']' are literal text — a rendered slice, a
	// progress tag, a shell comment — and must survive verbatim. Feeding them to
	// golor's scanner eats brackets and truncates the line at the first '#'.
	wrapper := &Wrapper{}
	cases := []string{
		"pids=[1 2 3]",
		"rm -rf ~/.codefly/data/<ws>    # e.g. mind-server",
		"[1/5] building",
		"error=Cannot connect [daemon]: is docker running? # hint",
	}
	for _, msg := range cases {
		got := stripANSI(wrapper.View("#(magenta)", "%s", msg))
		if got != msg {
			t.Errorf("View mangled message\n got:  %q\n want: %q", got, msg)
		}
	}
}

func TestViewStillInterpolatesArgs(t *testing.T) {
	wrapper := &Wrapper{}
	got := stripANSI(wrapper.View("#(white)", "[%d/%d] %s", 1, 5, "go"))
	if want := "[1/5] go"; got != want {
		t.Errorf("View args: got %q, want %q", got, want)
	}
}

func TestViewAppliesStyle(t *testing.T) {
	// The theme must still colorize — the styled output carries an SGR escape and,
	// once stripped, equals the plain message. The Codefly test runner captures
	// output through a pipe, so force color for this rendering-unit assertion
	// instead of depending on the ambient terminal.
	previousNoColor := color.NoColor
	color.NoColor = false
	t.Setenv("NO_COLOR", "")
	t.Cleanup(func() {
		color.NoColor = previousNoColor
	})
	wrapper := &Wrapper{}
	styled := wrapper.View("#(magenta)", "%s", "hello")
	if !strings.Contains(styled, "\x1b[") {
		t.Fatalf("expected ANSI styling in %q", styled)
	}
	if got := stripANSI(styled); got != "hello" {
		t.Errorf("stripped style: got %q, want %q", got, "hello")
	}
}

func TestUnwrapErrorLayers_Nil(t *testing.T) {
	if got := unwrapErrorLayers(nil); got != nil {
		t.Errorf("nil err: want nil, got %v", got)
	}
}

func TestOutputSinkReceivesAllDisplayLevels(t *testing.T) {
	var got []wool.Loglevel
	SetOutputSink(func(level wool.Loglevel, msg string) {
		got = append(got, level)
	})
	defer SetOutputSink(nil)

	Trace("trace")
	Debug("debug")
	Info("info")
	Warning("warn")
	Error("error")
	ErrorDetail("detail")
	Focus("focus")

	want := []wool.Loglevel{
		wool.TRACE,
		wool.DEBUG,
		wool.INFO,
		wool.WARN,
		wool.ERROR,
		wool.ERROR,
		wool.FOCUS,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sink levels got %#v, want %#v", got, want)
	}
}

func TestCaptureRecordsNarrationUntilDrained(t *testing.T) {
	StartCapture()
	defer DrainCapture() // ensure capturing is off if an assertion fails early

	Info("loading workspace")
	Warning("stale sweep failed")

	got := DrainCapture()
	want := []CapturedLine{
		{Level: wool.INFO, Message: "loading workspace"},
		{Level: wool.WARN, Message: "stale sweep failed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("captured got %#v, want %#v", got, want)
	}

	// After draining, capturing must be off — later narration is not recorded.
	Info("after the TUI is up")
	if again := DrainCapture(); len(again) != 0 {
		t.Fatalf("expected no capture after drain, got %#v", again)
	}
}

func TestCaptureIsTeeNotDiversion(t *testing.T) {
	// Capture must not swallow the line: with no output sink installed,
	// emitToSink still returns false so the helper takes its normal output
	// path. This is what keeps a load failure visible when it prints-and-exits
	// before the TUI starts.
	SetOutputSink(nil)
	StartCapture()
	defer DrainCapture()

	if emitToSink(wool.ERROR, "boom") {
		t.Fatal("emitToSink diverted the line while only capturing; want passthrough (false)")
	}
	if got := DrainCapture(); len(got) != 1 || got[0].Message != "boom" {
		t.Fatalf("capture got %#v, want one line %q", got, "boom")
	}
}

func TestRecordCaptureNoOpWhenNotCapturing(t *testing.T) {
	// Headless runs never StartCapture; emitToSink must not accumulate.
	DrainCapture() // make sure we start from a clean, non-capturing state
	emitToSink(wool.INFO, "headless line")
	if got := DrainCapture(); len(got) != 0 {
		t.Fatalf("expected nothing captured when not capturing, got %#v", got)
	}
}

func TestUnwrapErrorLayers_FlatError(t *testing.T) {
	err := errors.New("boom")
	got := unwrapErrorLayers(err)
	want := []string{"boom"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("flat: got %v, want %v", got, want)
	}
}

func TestUnwrapErrorLayers_MultiLayerStripsInnerSuffix(t *testing.T) {
	// Three-layer wrap with fmt.Errorf's "%w" — each layer's
	// rendered message includes its inner via "outer: inner".
	// The helper should strip that so each layer's row carries
	// only the OUTER's own contribution.
	root := errors.New("connection refused")
	mid := fmt.Errorf("postgres ping failed: %w", root)
	top := fmt.Errorf("service neo4j start: %w", mid)

	got := unwrapErrorLayers(top)
	want := []string{
		"service neo4j start",
		"postgres ping failed",
		"connection refused",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestUnwrapErrorLayers_PreservesLayerWithoutMatchingSuffix(t *testing.T) {
	// If an outer error's message DOESN'T end with ": "+inner
	// (e.g. it formatted the inner some other way), we should
	// keep the message verbatim — the strip is opportunistic.
	root := errors.New("root")
	custom := &customWrap{inner: root, msg: "totally different message"}
	got := unwrapErrorLayers(custom)
	if len(got) != 2 {
		t.Fatalf("want 2 layers, got %d: %v", len(got), got)
	}
	if got[0] != "totally different message" {
		t.Errorf("layer 0 should be preserved verbatim; got %q", got[0])
	}
	if got[1] != "root" {
		t.Errorf("layer 1 should be root; got %q", got[1])
	}
}

type customWrap struct {
	inner error
	msg   string
}

func (c *customWrap) Error() string { return c.msg }
func (c *customWrap) Unwrap() error { return c.inner }
