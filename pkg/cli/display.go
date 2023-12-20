package cli

import (
	"fmt"
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/codefly-dev/core/wool"
	"github.com/codefly-dev/golor"
)

func Header(level int, s string, templates ...any) {
	if len(s) == 0 {
		return
	}
	switch level {
	case 1:
		// Render a block of text.
		style := lipgloss.NewStyle()
		fmt.Println(style.Render(golor.Sprintf(fmt.Sprintf("#(bold,blue)[%s]", s), templates...)))
	case 2:
		// Render a block of text.
		style := lipgloss.NewStyle()
		fmt.Println(style.Render(golor.Sprintf(fmt.Sprintf("#(white)[%s]", s), templates...)))
	}
}

func Warning(s string, templates ...any) {
	// Render a block of text.
	style := lipgloss.NewStyle()
	fmt.Println(style.Render(golor.Sprintf(fmt.Sprintf("⚠️ #(bold,magenta)[%s]", s), templates...)))
}

func Debug(s string, templates ...any) {
	// Render a block of text.
	style := lipgloss.NewStyle()
	fmt.Println(style.Render(golor.Sprintf(fmt.Sprintf("#(bold,green)[DEBUG %s]", s), templates...)))
}

func Error(s string, templates ...any) {
	// Render a block of text.
	style := lipgloss.NewStyle()
	fmt.Println(style.Render(golor.Sprintf(fmt.Sprintf("☠️ #(bold,red)[%s]", s), templates...)))
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

func Exit() {
	os.Exit(0)
}
