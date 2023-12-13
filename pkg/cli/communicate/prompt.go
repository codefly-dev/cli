package communicate

import (
	"context"
	"fmt"

	agentsv1 "github.com/codefly-dev/core/proto/v1/go/agents"
	"github.com/codefly-dev/core/shared"
)

type Prompt struct{}

func NewPrompt() *Prompt {
	return &Prompt{}
}

func (h *Prompt) Answer(ctx context.Context, q *agentsv1.Question) (*agentsv1.Answer, error) {
	logger := shared.GetLogger(ctx).With("communicate")
	logger.Debugf("Processing request: %v", q)
	switch v := q.Value.(type) {
	case *agentsv1.Question_Display:

		return Display(q.Message, v.Display)
	case *agentsv1.Question_Confirm:
		return Confirm(q.Message, v.Confirm)
	default:
		return nil, fmt.Errorf("unknown question type: %v", q.Value)
	}
}
