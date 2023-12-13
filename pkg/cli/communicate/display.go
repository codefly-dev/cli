package communicate

// Handle Communicate Requests

import (
	"fmt"

	agentsv1 "github.com/codefly-dev/core/proto/v1/go/agents"

	"github.com/charmbracelet/lipgloss"
	"github.com/codefly-dev/golor"
)

func Display(msg *agentsv1.Message, data *agentsv1.Display) (*agentsv1.Answer, error) {
	// Render a block of text.
	var style = lipgloss.NewStyle().
		Margin(1, 2, 1, 2)

	fmt.Println(style.Render(golor.Sprintf(msg.Message, data.Data)))
	return &agentsv1.Answer{}, nil
}
