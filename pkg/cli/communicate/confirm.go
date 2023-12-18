package communicate

import (
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	agentv1 "github.com/codefly-dev/core/generated/go/services/agent/v1"
)

func Confirm(msg *agentv1.Message, c *agentv1.Confirm) (*agentv1.Answer, error) {
	cli.Header(2, msg.Description)
	confirm := models.Confirm(msg.Message, c.Default)
	return &agentv1.Answer{
		Value: &agentv1.Answer_Confirm{
			Confirm: &agentv1.ConfirmAnswer{
				Confirmed: confirm,
			},
		},
	}, nil
}
