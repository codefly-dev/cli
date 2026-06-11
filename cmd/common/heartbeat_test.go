package common

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWithHeartbeat_ReturnsResult(t *testing.T) {
	want := errors.New("boom")
	got := WithHeartbeat(context.Background(), "working", func() error {
		return want
	})
	if !errors.Is(got, want) {
		t.Fatalf("WithHeartbeat returned %v, want %v", got, want)
	}

	if err := WithHeartbeat(context.Background(), "working", func() error { return nil }); err != nil {
		t.Fatalf("WithHeartbeat returned %v, want nil", err)
	}
}

func TestWithHeartbeat_ReturnsPromptly(t *testing.T) {
	// A fast fn must return immediately — the heartbeat goroutine must not hold
	// the call open until the next tick.
	done := make(chan struct{})
	go func() {
		_ = WithHeartbeat(context.Background(), "working", func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WithHeartbeat did not return promptly for a fast fn")
	}
}
