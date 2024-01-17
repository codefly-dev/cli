package communicate

import (
	"context"

	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
	"github.com/codefly-dev/core/wool"
)

type Prompt struct{}

func NewPrompt() *Prompt {
	return &Prompt{}
}

func (h *Prompt) Answer(ctx context.Context, q *agentv0.Question) (*agentv0.Answer, error) {
	w := wool.Get(ctx).In("communicate.Answer")
	w.Trace("processing", wool.RequestField(q))
	switch v := q.Value.(type) {
	case *agentv0.Question_Display:

		return Display(q.Message, v.Display)
	case *agentv0.Question_Confirm:
		return Confirm(q.Message, v.Confirm)
	case *agentv0.Question_Input:
		return Input(q.Message, v.Input)
	default:
		return nil, w.NewError("unknown question type: %v", q.Value)
	}
}
