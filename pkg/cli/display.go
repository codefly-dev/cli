package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/codefly-dev/core/tui"
	"github.com/codefly-dev/golor"
)

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
	theme := "#(italic,green)"
	fmt.Fprintln(os.Stderr, tui.RenderTrace(wrapper.View(theme, s, args...)))
}

func Trace(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Trace(s, args...)
}

func (wrapper *Wrapper) Debug(s string, args ...any) {
	theme := "#(green)"
	fmt.Fprintln(os.Stderr, tui.RenderDebug(wrapper.View(theme, s, args...)))
}

func Debug(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Debug(s, args...)
}

func (wrapper *Wrapper) Info(s string, args ...any) {
	theme := "#(magenta)"
	fmt.Println(tui.RenderInfo(wrapper.View(theme, s, args...)))
}

func Info(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Info(s, args...)
}

func (wrapper *Wrapper) Error(s string, args ...any) {
	theme := "#(bold,red)"
	fmt.Fprintln(os.Stderr, tui.RenderError(wrapper.View(theme, s, args...)))
}

func (wrapper *Wrapper) ErrorDetail(s string, args ...any) {
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
	theme := "#(bold,red)"
	fmt.Println(tui.RenderFocus(wrapper.View(theme, s, args...)))
}

func Focus(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Focus(s, args...)
}

func ExitOnError(err error, format string, args ...any) {
	if err != nil {
		Error(format, args...)
		printErrorChain(err)
		ExitError()
	}
}

// ErrorChain prints a headline followed by the wrapped error rendered as a
// vertical chain (root cause highlighted), WITHOUT exiting. Use for failures
// that must still run cleanup/shutdown afterwards — e.g. a service that failed
// to start but whose containers and agents still need to be torn down.
func ErrorChain(err error, format string, args ...any) {
	Error(format, args...)
	printErrorChain(err)
}

// printErrorChain renders a wrapped error as a vertical chain so the
// root cause is always visible — not buried past terminal width.
// Each `errors.Wrap` layer becomes its own line, indented to show
// nesting; the innermost (root cause) is printed bold in red so the
// user knows exactly what blew up. Prior layers are dim so the eye
// flows straight to the bottom.
func printErrorChain(err error) {
	layers := unwrapErrorLayers(err)
	if len(layers) == 0 {
		return
	}
	// Render each layer on its own row. Last layer = root cause.
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

func ExitIf(b bool, format string, args ...any) {
	if b {
		Error(format, args...)
		ExitError()
	}
}

func ExitWithMessage(format string, args ...any) {
	Error(format, args...)
	ExitError()
}

func Exit() {
	Done()
	os.Exit(0)
}

func ExitError() {
	// Point the user at the detailed log on every failure exit — the single
	// biggest debuggability win: they no longer have to know that
	// ~/.codefly/logs/<date>.log exists. Printed to stderr, dim, once.
	if p := LogFilePath(); p != "" {
		fmt.Fprintln(os.Stderr, tui.RenderErrorDetail(fmt.Sprintf("↳ full logs: %s   (re-run with --debug for more)", p)))
	}
	Done()
	os.Exit(1)
}
