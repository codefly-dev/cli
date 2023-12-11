package communicate

import (
	"context"
	"fmt"

	agentsv1 "github.com/codefly-dev/core/proto/v1/go/agents"

	"github.com/codefly-dev/core/shared"
)

type CliHandler struct{}

func (h *CliHandler) Process(ctx context.Context, req *agentsv1.InformationRequest) (*agentsv1.Answer, error) {
	logger := shared.GetLogger(ctx).With("communicate")
	logger.Debugf("Processing request: %v", req)
	switch v := req.Question.Value.(type) {
	case *agentsv1.Question_Display:
		return Display(req.Question.Message, v.Display)
	case *agentsv1.Question_Confirm:
		return Confirm(req.Question.Message, v.Confirm)
	default:
		return nil, fmt.Errorf("unknown question type: %v", req.Question.Value)
	}
}
