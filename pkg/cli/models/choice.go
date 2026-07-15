package models

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/tui"
)

func Choice(ctx context.Context, msg string, all []*Entry) (*Entry, error) {
	if len(all) == 0 {
		return nil, fmt.Errorf("no entries to choose from")
	}
	if cli.WithDefault() {
		for _, e := range all {
			if e.Current {
				return e, nil
			}
		}
		return all[0], nil
	}
	result, err := tui.RunChoice(msg, all)
	if err != nil {
		return nil, err
	}
	if result.Stopped {
		return nil, ErrPromptCancelled
	}
	if result.Entry == nil {
		return nil, fmt.Errorf("choice prompt returned no selection")
	}
	return result.Entry, nil
}
