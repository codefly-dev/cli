package communicate

// Handle Communicate Requests

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	"github.com/codefly-dev/golor"
)

func Display(msg *corev1.Message, data *corev1.Display) (*corev1.Answer, error) {
	// Render a block of text.
	var style = lipgloss.NewStyle().
		Margin(1, 2, 1, 2)

	fmt.Println(style.Render(golor.Sprintf(msg.Message, data.Data)))
	return &corev1.Answer{}, nil
}
