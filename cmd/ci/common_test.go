package ci

import (
	"errors"
	"strings"
	"testing"
)

type recordingStopper struct {
	err   error
	calls int
}

func (s *recordingStopper) Stop() error {
	s.calls++
	return s.err
}

func TestRunAndStopFlowStopsExactlyOnce(t *testing.T) {
	flow := &recordingStopper{}
	if err := runAndStopFlow(flow, func() error { return nil }); err != nil {
		t.Fatalf("runAndStopFlow: %v", err)
	}
	if flow.calls != 1 {
		t.Fatalf("Stop calls = %d, want 1", flow.calls)
	}
}

func TestRunAndStopFlowRetainsActionAndStopErrors(t *testing.T) {
	actionErr := errors.New("action failed")
	flow := &recordingStopper{err: errors.New("stop failed")}
	err := runAndStopFlow(flow, func() error { return actionErr })
	if !errors.Is(err, actionErr) {
		t.Fatalf("result %v does not retain action error", err)
	}
	if !strings.Contains(err.Error(), "stop failed") {
		t.Fatalf("result %v does not retain stop error", err)
	}
	if flow.calls != 1 {
		t.Fatalf("Stop calls = %d, want 1", flow.calls)
	}
}

func TestRunAndStopFlowStopsDuringPanic(t *testing.T) {
	flow := &recordingStopper{}
	defer func() {
		if recover() == nil {
			t.Fatal("panic was swallowed")
		}
		if flow.calls != 1 {
			t.Fatalf("Stop calls = %d, want 1", flow.calls)
		}
	}()
	_ = runAndStopFlow(flow, func() error { panic("boom") })
}
