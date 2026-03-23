package models

import (
	"os"

	"github.com/codefly-dev/core/tui"
)

type Entry = tui.Entry

func Select(msg string, all []*Entry) (*Entry, error) {
	result, err := tui.RunSelect(msg, all)
	if err != nil {
		return nil, err
	}
	if result.Stopped {
		os.Exit(0)
	}
	return result.Entry, nil
}
