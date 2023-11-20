package communicate

// Handle Communicate Requests

import (
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	"github.com/codefly-dev/golor"
)

type ConfirmModel struct {
	confirmed   bool
	Prompt      string
	def         bool
	Description string
}

func (m ConfirmModel) Init() tea.Cmd {
	return nil
}

func (m ConfirmModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.KeyMsg:
		// Handle Ctrl+C and Ctrl+D
		if msg.Type == tea.KeyCtrlC || msg.Type == tea.KeyCtrlD {
			return m, tea.Quit
		}
		if msg.Type == tea.KeyEnter {
			m.confirmed = m.def
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

func (m ConfirmModel) View() string {
	// Render a block of text.
	var style = lipgloss.NewStyle().
		Margin(1, 2, 1, 2)
	return style.Render(golor.Sprintf("#(bold,magenta)[{{.Description}}\n{{.Prompt}}] [y/n]", m))
}

func Confirm(msg *corev1.Message, c *corev1.Confirm) (*corev1.Answer, error) {
	// Catch interrupt signal

	p := tea.NewProgram(ConfirmModel{
		Prompt:      msg.Message,
		Description: msg.Description,
		def:         c.Default,
	})
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT)
	go func() {
		<-ch
		p.Quit()
	}()
	mod, err := p.Run()
	if err != nil {
		return nil, err
	}
	m := mod.(ConfirmModel)
	return &corev1.Answer{
		Value: &corev1.Answer_Confirm{
			Confirm: &corev1.ConfirmAnswer{
				Confirmed: m.confirmed,
			},
		},
	}, nil
}
