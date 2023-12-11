package prompts

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/codefly-dev/core/shared"
)

type input struct {
	Message      string
	input        string
	defaultValue string
	stopped      bool
}

func (m input) Init() tea.Cmd {
	m.input = m.defaultValue
	return nil
}

func (m input) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyRunes:
			// If the current input is the default value, replace it
			if m.input == m.defaultValue {
				m.input = ""
			}
			m.input += string(msg.Runes)
		case tea.KeyBackspace:
			if len(m.input) > 0 {
				m.input = m.input[:len(m.input)-1]
			}
		case tea.KeyEnter:
			// If input is empty and default is present, use the default value
			if m.input == "" {
				m.input = m.defaultValue
			}
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
		Message:      msg,
		defaultValue: suggestion,
	})
	mod, err := p.Run()
	shared.UnexpectedExitOnError(err, "cannot run input prompt")
	m := mod.(input)
	if m.stopped {
		os.Exit(0)
	}
	return m.input
}
