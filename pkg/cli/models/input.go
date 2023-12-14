package models

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/codefly-dev/core/shared"
)

type input struct {
	Message string
	input   string
	stopped bool
}

func (m input) Init() tea.Cmd {
	return nil
}

func (m input) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyRunes:
			m.input += string(msg.Runes)
		case tea.KeyBackspace:
			if len(m.input) > 0 {
				m.input = m.input[:len(m.input)-1]
			}
		case tea.KeyEnter:
			return m, tea.Quit
		case tea.KeyCtrlC:
			m.stopped = true
			return m, tea.Quit
		default:
			m.stopped = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m input) View() string {
	return fmt.Sprintf("%s:\n%s", m.Message, m.input)
}

func Input(msg string, suggestion string) string {
	p := tea.NewProgram(input{
		Message: msg,
		input:   suggestion,
	})
	mod, err := p.Run()
	shared.UnexpectedExitOnError(err, "cannot run input prompt")
	m := mod.(input)
	if m.stopped {
		os.Exit(0)
	}
	return m.input
}
