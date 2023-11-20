package services

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
)

type model struct {
	confirmed bool
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y":
			m.confirmed = true
			return m, tea.Quit
		case "n", "q", "esc":
			m.confirmed = false
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m model) View() string {
	if m.confirmed {
		return "You chose to override the file.\n"
	}
	return golor.Sprintf("#(bold,green)[Service already found]. Override it? [y/n]: ")
}

func Override() (bool, error) {
	if shared.Override() {
		return true, nil
	}
	p := tea.NewProgram(model{})
	mod, err := p.Run()
	if err != nil {
		return false, err
	}
	m := mod.(model)
	return m.confirmed, nil
}
