package communicate

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	agentv1 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v1"
)

func Choice(ctx context.Context, msg *agentv1.Message, c *agentv1.Choice) (*agentv1.Answer, error) {
	cli.Header(2, "%s", msg.Description)
	entries := make([]*models.Entry, 0, len(c.Options))
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
	return &agentv1.Answer{
		Value: &agentv1.Answer_Choice{
			Choice: &agentv1.ChoiceAnswer{
				Option: toNames[input.Identifier],
			},
		},
	}, nil
}
