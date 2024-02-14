package models

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/codefly-dev/golor"
)

type choice struct {
	Message       string
	entries       []*Entry
	cursor        int // the index of the currently selected entry
	selectedEntry *Entry
	stopped       bool
}

func (m choice) Init() tea.Cmd {
	return nil
}

func (m choice) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.KeyMsg:
		switch msg.String() {
		case "j", "down":
			if m.cursor < len(m.entries)-1 {
				m.cursor++
			}
		case "k", "up":
			if m.cursor > 0 {
				m.cursor--
			}
		case "enter":
			m.selectedEntry = m.entries[m.cursor]
			return m, tea.Quit
		case "ctrl+c":
			m.stopped = true
			return m, tea.Quit
		}

	}
	return m, nil
}

func (m choice) View() string {
	s := golor.Template(m).Sprintf(`#(blue,bold)[{{.Message}}]`)
	s += "\n"
	for i, entry := range m.entries {
		cursor := " " // no cursor
		if m.cursor == i {
			cursor = ">" // cursor
		}
		s += fmt.Sprintf("%s %s\n", cursor, entry)
	}
	return s
}

func Choice(msg string, all []*Entry) (*Entry, error) {
	p := tea.NewProgram(choice{
		Message: msg,
		entries: all,
	})
	mod, err := p.Run()
	if err != nil {
		return nil, err
	}
	m := mod.(choice)
	if m.stopped {
		os.Exit(0)
	}
	return m.selectedEntry, nil
}
