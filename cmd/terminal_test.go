package cmd

import (
	"context"
	"errors"
	"testing"
)

func TestTerminalCommandReturnsErrors(t *testing.T) {
	if TerminalCmd.RunE == nil || TerminalCmd.Run != nil {
		t.Fatal("terminal command must return errors through RunE")
	}
	if err := TerminalCmd.Args(TerminalCmd, []string{"extra"}); err == nil {
		t.Fatal("terminal command accepted positional arguments")
	}
}

func TestReadTerminalInputHonorsCancellationBeforePolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readTerminalInput(ctx, -1, make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("readTerminalInput error = %v, want context cancellation", err)
	}
}
