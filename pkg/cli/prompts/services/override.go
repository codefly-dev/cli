package services

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
)

type override struct {
	confirmed bool
}

func (m override) Init() tea.Cmd {
	return nil
}

func (m override) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.Type == tea.KeyEnter {
			m.confirmed = true
			return m, tea.Quit
		}
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

func (m override) View() string {
	if m.confirmed {
		return "You chose to override the file.\n"
	}
	return golor.Sprintf("#(bold,green)[Service already found]. Replace it? [y/n]: ")
}

func Override() (bool, error) {
	if shared.GlobalOverride() {
		return true, nil
	}
	p := tea.NewProgram(override{})
	mod, err := p.Run()
	if err != nil {
		return false, err
	}
	m := mod.(override)
	return m.confirmed, nil
}
