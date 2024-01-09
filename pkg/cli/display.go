package cli

import (
	"fmt"
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/codefly-dev/core/wool"
	"github.com/codefly-dev/golor"
)

func View(style string, s string, args ...any) string {
	view := fmt.Sprintf(s, args...)
	view = fmt.Sprintf("%s[%s]", style, view)
	view = golor.Sprintf(view)
	return view
}

func Header(level int, s string, args ...any) {
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
	fmt.Println(style.Render(View(theme, s, args...)))
}

func Warning(s string, args ...any) {
	theme := "⚠️ #(bold,magenta)"
	style := lipgloss.NewStyle()
	fmt.Println(style.Render(View(theme, s, args...)))
}

func Trace(s string, args ...any) {
	theme := "#(italic,green)"
	style := lipgloss.NewStyle()
	fmt.Println(style.Render(View(theme, s, args...)))
}

func Debug(s string, args ...any) {
	theme := "#(green)"
	style := lipgloss.NewStyle()
	fmt.Println(style.Render(View(theme, s, args...)))
}

func Info(s string, args ...any) {
	theme := "#(magenta)"
	style := lipgloss.NewStyle()
	fmt.Println(style.Render(View(theme, s, args...)))
}

func Error(s string, args ...any) {
	theme := "☠️ #(bold,red)"
	style := lipgloss.NewStyle()
	fmt.Println(style.Render(View(theme, s, args...)))
}

func Focus(s string, args ...any) {
	theme := "#(bold,red)"
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder())
	fmt.Println(style.Render(View(theme, s, args...)))
}

func ExitOnError(err error, format string, args ...any) {
	if err != nil {
		Error(format, args...)
		if wool.IsDebug() {
			Error(err.Error())
		}
		Exit()
	}
}

func ExitWithMessage(format string, args ...any) {
	Error(format, args...)
	Exit()
}

func Exit() {
	os.Exit(0)
}
