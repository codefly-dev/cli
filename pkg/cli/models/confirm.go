package models

import (
	"context"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/tui"
)

func Confirm(ctx context.Context, s string, defaultValue bool) bool {
	confirmed, _ := ConfirmE(ctx, s, defaultValue)
	return confirmed
}

// ConfirmE distinguishes an explicit negative answer from Ctrl+C. Command
// call sites that treat both as a clean decline can use Confirm; RPC prompt
// bridges should propagate ErrPromptCancelled to their caller.
func ConfirmE(ctx context.Context, s string, defaultValue bool) (bool, error) {
	if cli.WithDefault() {
		return defaultValue, nil
	}
	result, err := tui.RunConfirm(s, defaultValue)
	if err != nil {
		return false, err
	}
	if result.Stopped {
		common.Cancel(ctx)
		return false, ErrPromptCancelled
	}
	return result.Confirmed, nil
}
