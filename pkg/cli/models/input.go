package models

import (
	"os"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/tui"
)

func Input(msg string, defaultValue string) string {
	if cli.WithDefault() {
		return defaultValue
	}
	result, err := tui.RunInput(msg, defaultValue)
	cli.ExitOnError(err, "cannot run Input prompt")
	if result.Stopped {
		os.Exit(0)
	}
	return result.Value
}
