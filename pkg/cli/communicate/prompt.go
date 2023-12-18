package communicate

import (
	"context"
	"fmt"

	agentv1 "github.com/codefly-dev/core/generated/go/services/agent/v1"
	"github.com/codefly-dev/core/wool"
)

type Prompt struct{}

func NewPrompt() *Prompt {
	return &Prompt{}
}

func (h *Prompt) Answer(ctx context.Context, q *agentv1.Question) (*agentv1.Answer, error) {
	w := wool.Get(ctx).In("communicate")
	logger.Debugf("Processing request: %v", q)
	switch v := q.Value.(type) {
	case *agentv1.Question_Display:

		return Display(q.Message, v.Display)
	case *agentv1.Question_Confirm:
		return Confirm(q.Message, v.Confirm)
	default:
		return nil, fmt.Errorf("unknown question type: %v", q.Value)
	}
}
