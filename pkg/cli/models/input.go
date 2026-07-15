package models

import (
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/tui"
)

func Input(msg string, defaultValue string) (string, error) {
	if cli.WithDefault() {
		return defaultValue, nil
	}
	result, err := tui.RunInput(msg, defaultValue)
	if err != nil {
		return "", err
	}
	if result.Stopped {
		return "", ErrPromptCancelled
	}
	return result.Value, nil
}
