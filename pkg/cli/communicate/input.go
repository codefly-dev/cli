package communicate

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
)

func Input(ctx context.Context, msg *agentv0.Message, c *agentv0.Input) (*agentv0.Answer, error) {
	cli.Header(2, msg.Description)
	// TODO: Deal with int
	input := models.Input(msg.Message, c.GetStringDefault())
	return &agentv0.Answer{
		Value: &agentv0.Answer_Input{
			Input: &agentv0.InputAnswer{
				Answer: &agentv0.InputAnswer_StringValue{StringValue: input},
			},
		},
	}, nil
}
