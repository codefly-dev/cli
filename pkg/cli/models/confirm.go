package models

import (
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/golor"
)

type ConfirmModel struct {
	Message string
	Options string

	confirmed bool
	stopped   bool
}

func (m ConfirmModel) Init() tea.Cmd {
	return nil
}

func (m ConfirmModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Handle Ctrl+C and Ctrl+D
		if msg.Type == tea.KeyCtrlC || msg.Type == tea.KeyCtrlD {
			m.stopped = true
			return m, tea.Quit
		}
		if msg.Type == tea.KeyEnter {
			return m, tea.Quit
		}
		switch msg.String() {
		case "y", "Y":
			m.confirmed = true
			return m, tea.Quit
		case "n", "N", "q", "esc":
			m.confirmed = false
			return m, tea.Quit

		}
	}
	return m, nil
}

func (m ConfirmModel) View() string {
	// Render a block of text.
	style := lipgloss.NewStyle().
		Margin(1, 2, 1, 2)
	return style.Render(golor.Template(m).Sprintf("#(bold,magenta)[{{.Message}} {{.Options}} ]"))
}

func DefaultInput(def bool) string {
	if def {
		return "(Y/n)"
	}
	return "(y/N)"
}

func Confirm(s string, defaultValue bool) bool {
	if cli.WithDefault() {
		return defaultValue
	}
	p := tea.NewProgram(ConfirmModel{
		Message:   s,
		Options:   DefaultInput(defaultValue),
		confirmed: defaultValue,
	})
	mod, err := p.Run()
	if err != nil {
		return false
	}
	m := mod.(ConfirmModel)
	if m.stopped {
		os.Exit(0)
	}
	return m.confirmed
}
