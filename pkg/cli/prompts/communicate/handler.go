package communicate

import (
	"fmt"

	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	"github.com/codefly-dev/core/shared"
)

type CliHandler struct{}

func (h *CliHandler) Process(req *corev1.InformationRequest) (*corev1.Answer, error) {
	logger := shared.NewLogger("communicate")
	logger.Debugf("Processing request: %v", req)
	switch v := req.Question.Value.(type) {
	case *corev1.Question_Display:
		return Display(req.Question.Message, v.Display)
	case *corev1.Question_Confirm:
		return Confirm(req.Question.Message, v.Confirm)
	default:
		return nil, fmt.Errorf("unknown question type: %v", req.Question.Value)
	}
}
