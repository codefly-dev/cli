package common

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestContextFromSignalsCancelsFirstAndForcesSecond(t *testing.T) {
	signals := make(chan os.Signal, 2)
	exits := make(chan int, 1)
	ctx, stop := contextFromSignals(context.Background(), signals, func(code int) { exits <- code })
	defer stop()

	signals <- os.Interrupt
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("first signal did not cancel context")
	}
	select {
	case code := <-exits:
		t.Fatalf("first signal forced exit %d", code)
	case <-time.After(100 * time.Millisecond):
	}

	signals <- syscall.SIGTERM
	select {
	case code := <-exits:
		if code != 130 {
			t.Fatalf("force exit code = %d, want 130", code)
		}
	case <-time.After(time.Second):
		t.Fatal("second signal did not force exit")
	}
}

func TestContextFromSignalsStopIsIdempotent(t *testing.T) {
	ctx, stop := contextFromSignals(context.Background(), make(chan os.Signal), func(int) {
		t.Fatal("unexpected force exit")
	})
	stop()
	stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel context")
	}
}
