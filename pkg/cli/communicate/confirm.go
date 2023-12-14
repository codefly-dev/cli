package communicate

import (
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	agentsv1 "github.com/codefly-dev/core/generated/v1/go/proto/agents"
)

func Confirm(msg *agentsv1.Message, c *agentsv1.Confirm) (*agentsv1.Answer, error) {
	cli.Header(2, msg.Description)
	confirm := models.Confirm(msg.Message, c.Default)
	return &agentsv1.Answer{
		Value: &agentsv1.Answer_Confirm{
			Confirm: &agentsv1.ConfirmAnswer{
				Confirmed: confirm,
			},
		},
	}, nil
}
