package cli

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
	"github.com/codefly-dev/golor"
)

func Header(level int, s string, templates ...any) {
	if len(s) == 0 {
		return
	}
	switch level {
	case 1:
		// Render a block of text.
		style := lipgloss.NewStyle().
			Margin(1, 2, 1, 2)
		fmt.Println(style.Render(golor.Sprintf(fmt.Sprintf("#(bold,magenta)[%s]", s), templates...)))
	case 2:
		// Render a block of text.
		style := lipgloss.NewStyle().
			Margin(1, 2, 1, 2)
		fmt.Println(style.Render(golor.Sprintf(fmt.Sprintf("#(white)[%s]", s), templates...)))
	}
}

func Warning(s string, templates ...any) {
	// Render a block of text.
	style := lipgloss.NewStyle().
		Margin(1, 2, 1, 2)
	fmt.Println(style.Render(golor.Sprintf(fmt.Sprintf("#(bold,magenta)[%s]", s), templates...)))
}

func Error(s string, templates ...any) {
	// Render a block of text.
	style := lipgloss.NewStyle().
		Margin(1, 2, 1, 2)
	fmt.Println(style.Render(golor.Sprintf(fmt.Sprintf("#(bold,magenta)[%s]", s), templates...)))
}
