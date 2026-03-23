package models

import (
	"context"
	"fmt"
	"os"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/tui"
)

func Choice(ctx context.Context, msg string, all []*Entry) (*Entry, error) {
	if cli.WithDefault() {
		for _, e := range all {
			if e.Current {
				return e, nil
			}
		}
		if len(all) > 0 {
			return all[0], nil
		}
		return nil, fmt.Errorf("no entries to choose from")
	}
	result, err := tui.RunChoice(msg, all)
	if err != nil {
		return nil, err
	}
	if result.Stopped {
		os.Exit(0)
	}
	return result.Entry, nil
}
