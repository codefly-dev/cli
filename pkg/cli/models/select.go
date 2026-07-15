package models

import (
	"fmt"

	"github.com/codefly-dev/core/tui"
)

type Entry = tui.Entry

func Select(msg string, all []*Entry) (*Entry, error) {
	if len(all) == 0 {
		return nil, fmt.Errorf("no entries to select from")
	}
	result, err := tui.RunSelect(msg, all)
	if err != nil {
		return nil, err
	}
	if result.Stopped {
		return nil, ErrPromptCancelled
	}
	if result.Entry == nil {
		return nil, fmt.Errorf("select prompt returned no selection")
	}
	return result.Entry, nil
}
