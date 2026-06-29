package communicate

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
)

func Confirm(ctx context.Context, msg *agentv0.Message, c *agentv0.Confirm) (*agentv0.Answer, error) {
	cli.Header(2, "%s", msg.Description)
	confirm := models.Confirm(ctx, msg.Message, c.Default)
	return &agentv0.Answer{
		Value: &agentv0.Answer_Confirm{
			Confirm: &agentv0.ConfirmAnswer{
				Confirmed: confirm,
			},
		},
	}, nil
}
