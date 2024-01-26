package models

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/codefly-dev/golor"
)

type Entry struct {
	Identifier  string
	Current     bool
	Description string
}

type selection struct {
	Message       string
	entries       []*Entry
	cursor        int // the index of the currently selected entry
	selectedEntry *Entry
	stopped       bool
}

func (m selection) Init() tea.Cmd {
	return nil
}

func (m selection) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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

func (entry *Entry) String() string {
	display := entry.Identifier
	if entry.Current {
		display += " (active)"
	}
	return golor.Template(display).Sprintf(`#(blue,bold)[{{.}}]`)
}

func (m selection) View() string {
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

func Select(msg string, all []*Entry) (*Entry, error) {
	p := tea.NewProgram(selection{
		Message: msg,
		entries: all,
	})
	mod, err := p.Run()
	if err != nil {
		return nil, err
	}
	m := mod.(selection)
	if m.stopped {
		os.Exit(0)
	}
	return m.selectedEntry, nil
}
