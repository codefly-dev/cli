package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/codefly-dev/core/tui"
	"github.com/codefly-dev/core/wool"
	"github.com/codefly-dev/golor"
)

// outputSink, when set, receives codefly's own narration (Info/Header/Warning)
// as plain text instead of letting it go to stdout/stderr. The interactive run
// sets it while a full-screen TUI owns the terminal, so progress like "Handling
// <frontend> with these dependent services: …" lands in the scrollable log pane
// rather than being painted to stdout and instantly overwritten by the alt
// screen (which is why it looked like nothing was happening for ~25s on start).
var outputSink atomic.Pointer[func(level wool.Loglevel, msg string)]

// SetOutputSink installs (or, with nil, removes) the narration sink.
func SetOutputSink(fn func(level wool.Loglevel, msg string)) {
	if fn == nil {
		outputSink.Store(nil)
		return
	}
	outputSink.Store(&fn)
}

// emitToSink forwards a line to the sink if one is installed, returning true
// when it handled the line (so the caller skips its stdout/stderr write).
func emitToSink(level wool.Loglevel, msg string) bool {
	recordCapture(level, msg)
	if p := outputSink.Load(); p != nil {
		(*p)(level, msg)
		return true
	}
	return false
}

// CapturedLine is one narration line recorded during the pre-TUI startup
// window, kept so it can be replayed into the log pane once the interactive
// TUI owns the terminal.
type CapturedLine struct {
	Level   wool.Loglevel
	Message string
}

// capturing tees narration into `captured` while the interactive run is still
// doing its pre-flow work (stale-process reaping, workspace load) before the
// TUI exists. It is a TEE, not a diversion: lines still take their normal
// stdout/stderr path, so a load failure that prints-and-exits before the TUI
// starts stays visible. DrainCapture replays the buffer into the log pane.
var (
	capturing atomic.Bool
	captureMu sync.Mutex
	captured  []CapturedLine
)

// StartCapture begins recording narration lines in addition to their normal
// output. Called at the very start of an interactive run so the startup window
// can be shown in the TUI log pane instead of being painted to a terminal the
// alt screen then erases.
func StartCapture() {
	captureMu.Lock()
	captured = nil
	captureMu.Unlock()
	capturing.Store(true)
}

// DrainCapture stops recording and returns everything captured since
// StartCapture, in emission order.
func DrainCapture() []CapturedLine {
	capturing.Store(false)
	captureMu.Lock()
	out := captured
	captured = nil
	captureMu.Unlock()
	return out
}

func recordCapture(level wool.Loglevel, msg string) {
	if !capturing.Load() {
		return
	}
	captureMu.Lock()
	captured = append(captured, CapturedLine{Level: level, Message: msg})
	captureMu.Unlock()
}

type Wrapper struct {
	template any
}

func (wrapper *Wrapper) View(style string, s string, args ...any) string {
	view := fmt.Sprintf("%s[%s]", style, s)
	return golor.Template(wrapper.template).Sprintf(view, args...)
}

func Template(t any) *Wrapper {
	return &Wrapper{template: t}
}

func (wrapper *Wrapper) Header(level int, s string, args ...any) {
	if len(s) == 0 {
		return
	}
	if emitToSink(wool.INFO, fmt.Sprintf(s, args...)) {
		return
	}
	var theme string
	switch level {
	case 1:
		theme = "#(white)"
	case 2:
		theme = "#(bold,blue)"
	}
	fmt.Println(tui.RenderHeader(level, wrapper.View(theme, s, args...)))
}

func Header(level int, s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Header(level, s, args...)
}

func (wrapper *Wrapper) Warning(s string, args ...any) {
	if emitToSink(wool.WARN, fmt.Sprintf(s, args...)) {
		return
	}
	theme := "#(bold,magenta)"
	// Diagnostics (warnings/errors/trace/debug) go to STDERR so they never
	// pollute stdout, which carries the command's real output — `$(codefly
	// endpoint ...)`, piped JSON, etc. Only Header/Info/Focus stay on stdout.
	fmt.Fprintln(os.Stderr, tui.RenderWarning(wrapper.View(theme, s, args...)))
}

func Warning(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Warning(s, args...)
}

func (wrapper *Wrapper) Trace(s string, args ...any) {
	if emitToSink(wool.TRACE, fmt.Sprintf(s, args...)) {
		return
	}
	theme := "#(italic,green)"
	fmt.Fprintln(os.Stderr, tui.RenderTrace(wrapper.View(theme, s, args...)))
}

func Trace(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Trace(s, args...)
}

func (wrapper *Wrapper) Debug(s string, args ...any) {
	if emitToSink(wool.DEBUG, fmt.Sprintf(s, args...)) {
		return
	}
	theme := "#(green)"
	fmt.Fprintln(os.Stderr, tui.RenderDebug(wrapper.View(theme, s, args...)))
}

func Debug(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Debug(s, args...)
}

func (wrapper *Wrapper) Info(s string, args ...any) {
	if emitToSink(wool.INFO, fmt.Sprintf(s, args...)) {
		return
	}
	theme := "#(magenta)"
	fmt.Println(tui.RenderInfo(wrapper.View(theme, s, args...)))
}

func Info(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Info(s, args...)
}

func (wrapper *Wrapper) Error(s string, args ...any) {
	if emitToSink(wool.ERROR, fmt.Sprintf(s, args...)) {
		return
	}
	theme := "#(bold,red)"
	fmt.Fprintln(os.Stderr, tui.RenderError(wrapper.View(theme, s, args...)))
}

func (wrapper *Wrapper) ErrorDetail(s string, args ...any) {
	if emitToSink(wool.ERROR, fmt.Sprintf(s, args...)) {
		return
	}
	theme := "#(bold,red)"
	fmt.Fprintln(os.Stderr, tui.RenderErrorDetail(wrapper.View(theme, s, args...)))
}

func Error(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Error(s, args...)
}

func ErrorDetail(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.ErrorDetail(s, args...)
}

func (wrapper *Wrapper) Focus(s string, args ...any) {
	if emitToSink(wool.FOCUS, fmt.Sprintf(s, args...)) {
		return
	}
	theme := "#(bold,red)"
	fmt.Println(tui.RenderFocus(wrapper.View(theme, s, args...)))
}

func Focus(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Focus(s, args...)
}

// ErrorChain prints a headline followed by the wrapped error rendered as a
// vertical chain (root cause highlighted), WITHOUT exiting. Use for failures
// that must still run cleanup/shutdown afterwards — e.g. a service that failed
// to start but whose containers and agents still need to be torn down.
func ErrorChain(err error, format string, args ...any) {
	Error(format, args...)
	printErrorChain(err)
	if path := LogFilePath(); path != "" {
		fmt.Fprintln(os.Stderr, tui.RenderErrorDetail(fmt.Sprintf("↳ full logs: %s   (re-run with --debug for more)", path)))
	}
}

// printErrorChain renders a wrapped error so the user sees what blew up.
//
// In normal mode we show ONLY the root cause — the innermost layer is what
// the user can actually act on ("docker daemon not reachable"), and the
// "↳ full logs … (re-run with --debug for more)" hint points anyone who
// needs the wrap chain at it. Dumping every `errors.Wrap` layer to the
// terminal on each failure is noise.
//
// In debug mode we render the full vertical chain (each layer on its own
// indented row, root cause highlighted) so the wrap path is visible without
// opening the log file.
func printErrorChain(err error) {
	layers := unwrapErrorLayers(err)
	if len(layers) == 0 {
		return
	}
	if !wool.IsDebug() {
		// Just the root cause, indented one level under the headline.
		root := layers[len(layers)-1]
		fmt.Fprintln(os.Stderr, tui.RenderError("  ↳ "+root))
		return
	}
	// Debug: render each layer on its own row. Last layer = root cause.
	for i, msg := range layers {
		indent := strings.Repeat("  ", i)
		marker := "↳"
		if i == 0 {
			marker = "·"
		}
		line := fmt.Sprintf("%s%s %s", indent, marker, msg)
		if i == len(layers)-1 {
			// Root cause — render with explicit color so it pops
			fmt.Fprintln(os.Stderr, tui.RenderError(line))
		} else {
			fmt.Fprintln(os.Stderr, tui.RenderErrorDetail(line))
		}
	}
}

// unwrapErrorLayers walks the Unwrap chain and returns each layer's
// OWN message — stripping the appended inner message that pkg/errors
// adds with its `outer: inner` rendering, so a 4-layer wrap renders
// as 4 distinct rows instead of one giant string repeating every
// ancestor.
//
// Returns nil for nil err. Pure so it's unit-testable without
// touching stdout / TUI.
func unwrapErrorLayers(err error) []string {
	var layers []string
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		msg := cur.Error()
		if inner := errors.Unwrap(cur); inner != nil {
			// pkg/errors / fmt.Errorf("%w") render as "outer: inner".
			// CutSuffix returns (msg-without-suffix, true) when the
			// suffix is present, leaving msg untouched if not — so a
			// custom wrap that formats its inner differently keeps
			// its full message verbatim.
			if trimmed, ok := strings.CutSuffix(msg, ": "+inner.Error()); ok {
				msg = trimmed
			}
		}
		layers = append(layers, msg)
	}
	return layers
}
