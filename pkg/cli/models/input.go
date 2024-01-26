package models

import (
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/golor"
)

type input struct {
	Message string
	Input   string
	stopped bool
}

func (m input) Init() tea.Cmd {
	return nil
}

func (m input) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeySpace:
			m.Input += " "
		case tea.KeyRunes:
			m.Input += string(msg.Runes)
		case tea.KeyBackspace:
			if len(m.Input) > 0 {
				m.Input = m.Input[:len(m.Input)-1]
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
	return golor.Template(m).Sprintf("#blue[{{.Message}}]:\n#white[{{.Input}}]")
}

func Input(msg string, defaultValue string) string {
	if cli.WithDefault() {
		return defaultValue
	}
	p := tea.NewProgram(input{
		Message: msg,
		Input:   defaultValue,
	})
	mod, err := p.Run()
	cli.ExitOnError(err, "cannot run Input prompt")
	m := mod.(input)
	if m.stopped {
		os.Exit(0)
	}
	return m.Input
}
