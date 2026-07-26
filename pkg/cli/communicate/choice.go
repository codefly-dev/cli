package communicate

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
)

func Choice(ctx context.Context, msg *agentv0.Message, c *agentv0.Choice) (*agentv0.Answer, error) {
	cli.Header(2, "%s", msg.Description)
	var entries []*models.Entry
	toNames := make(map[string]string)
	for _, option := range c.Options {
		entry := &models.Entry{
			Identifier:  option.Message,
			Description: option.Description,
			Current:     option.Name == c.GetDefaultOption(),
		}
		entries = append(entries, entry)
		toNames[entry.Identifier] = option.Name
	}
	input, err := models.Choice(ctx, msg.Message, entries)
	if err != nil {
		return nil, err
	}
	return &agentv0.Answer{
		Value: &agentv0.Answer_Choice{
			Choice: &agentv0.ChoiceAnswer{
				Option: toNames[input.Identifier],
			},
		},
	}, nil
}
