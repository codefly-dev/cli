package communicate

// Handle Communicate Requests

import (
	"fmt"

	agentv1 "github.com/codefly-dev/core/generated/go/services/agent/v1"

	"github.com/charmbracelet/lipgloss"
	"github.com/codefly-dev/golor"
)

func Display(msg *agentv1.Message, data *agentv1.Display) (*agentv1.Answer, error) {
	// Render a block of text.
	var style = lipgloss.NewStyle().
		Margin(1, 2, 1, 2)

	fmt.Println(style.Render(golor.Sprintf(msg.Message, data.Data)))
	return &agentv1.Answer{}, nil
}
