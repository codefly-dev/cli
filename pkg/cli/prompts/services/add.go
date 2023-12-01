package services

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/golor"
)

type add struct {
	confirmed bool
	agent     *configurations.Agent
	app       *configurations.Application
	name      string
}

func (m add) Init() tea.Cmd {
	return nil
}

func (m add) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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

func (m add) View() string {
	if m.confirmed {
		return "Let's create some magic for you!\n"
	}
	return golor.Sprintf("#(bold,green)[Want to add a service <{{.Service}}> based on the agent <{{.Agent.Identifier}}> in your application <{{.Application.Name}}>]? Y/n",
		map[string]interface{}{"Service": m.name, "Agent": m.agent, "Application": m.app})
}

func Add(name string, agent *configurations.Agent, app *configurations.Application) (bool, error) {
	p := tea.NewProgram(add{name: name, agent: agent, app: app})
	mod, err := p.Run()
	if err != nil {
		return false, err
	}
	m := mod.(add)
	return m.confirmed, nil
}
