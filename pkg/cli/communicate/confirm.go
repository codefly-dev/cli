package communicate

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	agentv1 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v1"
)

func Confirm(ctx context.Context, msg *agentv1.Message, c *agentv1.Confirm) (*agentv1.Answer, error) {
	cli.Header(2, "%s", msg.Description)
	confirm, err := models.ConfirmE(ctx, msg.Message, c.Default)
	if err != nil {
		return nil, err
	}
	return &agentv1.Answer{
		Value: &agentv1.Answer_Confirm{
			Confirm: &agentv1.ConfirmAnswer{
				Confirmed: confirm,
			},
		},
	}, nil
}
