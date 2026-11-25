package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

func TestStoppedTerminalSideReportsOnlyFailuresBeyondTheStop(t *testing.T) {
	failure := errors.New("stdin is gone")
	for name, tc := range map[string]struct {
		reported error
		want     error
	}{
		"nothing":               {},
		"cancellation":          {reported: fmt.Errorf("read terminal input: %w", context.Canceled)},
		"stream cancellation":   {reported: fmt.Errorf("receive terminal output: %w", status.Error(codes.Canceled, "context canceled"))},
		"send on closed stream": {reported: fmt.Errorf("send terminal input: %w", io.EOF)},
		"a real failure":        {reported: failure, want: failure},
		"a stream failure":      {reported: status.Error(codes.Unavailable, "gone"), want: status.Error(codes.Unavailable, "gone")},
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			done <- tc.reported
			got := stoppedTerminalSide(done)
			if (tc.want == nil) != (got == nil) || (tc.want != nil && got.Error() != tc.want.Error()) {
				t.Fatalf("stoppedTerminalSide(%v) = %v, want %v", tc.reported, got, tc.want)
			}
		})
	}
}
