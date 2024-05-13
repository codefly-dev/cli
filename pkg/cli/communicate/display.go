package communicate

// Handle Communicate Requests

import (
	"context"
	"fmt"

	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"

	"github.com/charmbracelet/lipgloss"
	"github.com/codefly-dev/golor"
)

func Display(ctx context.Context, msg *agentv0.Message, data *agentv0.Display) (*agentv0.Answer, error) {
	// Render a block of text.
	var style = lipgloss.NewStyle().
		Margin(1, 2, 1, 2)

	fmt.Println(style.Render(golor.Sprintf(msg.Message, data.Data)))
	return &agentv0.Answer{}, nil
}
