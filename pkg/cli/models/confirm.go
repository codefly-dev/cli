package models

import (
	"context"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/tui"
)

func Confirm(ctx context.Context, s string, defaultValue bool) bool {
	if cli.WithDefault() {
		return defaultValue
	}
	result, err := tui.RunConfirm(s, defaultValue)
	if err != nil {
		return false
	}
	if result.Stopped {
		common.Cancel(ctx)
		cli.Exit()
	}
	return result.Confirmed
}
