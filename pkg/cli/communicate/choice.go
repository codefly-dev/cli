package communicate

import (
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	agentv0 "github.com/codefly-dev/core/generated/go/services/agent/v0"
)

func Choice(msg *agentv0.Message, c *agentv0.Choice) (*agentv0.Answer, error) {
	cli.Header(2, msg.Description)
	var entries []*models.Entry
	for _, option := range c.Options {
		entries = append(entries, &models.Entry{
			Identifier:  option.Message,
			Description: option.Description,
		})
	}
	input, err := models.Choice(msg.Message, entries)
	if err != nil {
		return nil, err
	}
	return &agentv0.Answer{
		Value: &agentv0.Answer_Choice{
			Choice: &agentv0.ChoiceAnswer{
				Option: input.Identifier,
			},
		},
	}, nil
}
