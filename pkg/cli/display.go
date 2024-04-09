package cli

import (
	"fmt"
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/codefly-dev/golor"
)

// Deal with templates the same way as golor

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
	style := lipgloss.NewStyle()
	fmt.Println(style.Render(wrapper.View(theme, s, args...)))
}

func Header(level int, s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Header(level, s, args...)
}

func (wrapper *Wrapper) Warning(s string, args ...any) {
	theme := "⚠️ #(bold,magenta)"
	style := lipgloss.NewStyle()
	fmt.Println(style.Render(wrapper.View(theme, s, args...)))
}

func Warning(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Warning(s, args...)
}

func (wrapper *Wrapper) Trace(s string, args ...any) {
	theme := "#(italic,green)"
	style := lipgloss.NewStyle()
	fmt.Println(style.Render(wrapper.View(theme, s, args...)))
}

func Trace(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Trace(s, args...)
}

func (wrapper *Wrapper) Debug(s string, args ...any) {
	theme := "#(green)"
	style := lipgloss.NewStyle()
	fmt.Println(style.Render(wrapper.View(theme, s, args...)))
}

func Debug(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Debug(s, args...)
}

func (wrapper *Wrapper) Info(s string, args ...any) {
	theme := "#(magenta)"
	style := lipgloss.NewStyle()
	fmt.Println(style.Render(wrapper.View(theme, s, args...)))
}

func Info(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Info(s, args...)
}

func (wrapper *Wrapper) Error(s string, args ...any) {
	theme := "☠️ #(bold,red)"
	style := lipgloss.NewStyle()
	fmt.Println(style.Render(wrapper.View(theme, s, args...)))
}

func (wrapper *Wrapper) ErrorDetail(s string, args ...any) {
	theme := "#(bold,red)"
	style := lipgloss.NewStyle()
	fmt.Println(style.Render(wrapper.View(theme, s, args...)))
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
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder())
	fmt.Println(style.Render(wrapper.View(theme, s, args...)))
}

func Focus(s string, args ...any) {
	wrapper := &Wrapper{}
	wrapper.Focus(s, args...)
}

func ExitOnError(err error, format string, args ...any) {
	if err != nil {
		Error(format, args...)
		ErrorDetail(err.Error())
		ExitError()
	}
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
	Done()
	os.Exit(1)
}
