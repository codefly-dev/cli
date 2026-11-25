package communicate

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	agentv1 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v1"
)

func Input(_ context.Context, msg *agentv1.Message, c *agentv1.Input) (*agentv1.Answer, error) {
	cli.Header(2, "%s", msg.Description)
	// TODO: Deal with int
	input, err := models.Input(msg.Message, c.GetStringDefault())
	if err != nil {
		return nil, err
	}
	return &agentv1.Answer{
		Value: &agentv1.Answer_Input{
			Input: &agentv1.InputAnswer{
				Answer: &agentv1.InputAnswer_StringValue{StringValue: input},
			},
		},
	}, nil
}
