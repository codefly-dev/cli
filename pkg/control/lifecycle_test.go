package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/orchestration"
)

// These cover the lifecycle guards that resolve before any flow is built, so
// they stay hermetic. The happy paths (Build/Test/Run) spawn real plugin agents
// and belong in an integration test with an installed agent, per the repo's
// no-mock rule.

func TestBuildRejectsModuleWide(t *testing.T) {
	_, err := New().Build(context.Background(), BuildRequest{Module: "backend"})
	if err == nil {
		t.Fatal("expected module-wide build to be rejected")
	}
}

func TestBuildRejectsPush(t *testing.T) {
	_, err := New().Build(context.Background(), BuildRequest{Service: "backend/api", Push: true})
	if err == nil {
		t.Fatal("expected push to be rejected until registry plumbing is lifted")
	}
}

func TestStopIsNoopWhenNothingRunning(t *testing.T) {
	// A fresh plane owns an empty flow registry, so Stop is a no-op.
	stopped, err := New().Stop(context.Background(), StopRequest{})
	if err != nil {
		t.Fatalf("Stop with nothing running = %v, want nil", err)
	}
	if stopped {
		t.Fatal("Stop with nothing running reported stopped = true")
	}
}

// TestWaitReadyNamesThePendingRequirement covers the diagnostic a --wait
// timeout used to lack: giving up reported only the deadline, so a stack that
// never came up said nothing about which requirement was holding it.
func TestWaitReadyNamesThePendingRequirement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	err := waitReady(ctx, &orchestration.Flow{}, make(chan error))
	if err == nil {
		t.Fatal("expected waitReady to give up")
	}
	if !strings.Contains(err.Error(), "flow not ready") {
		t.Fatalf("error %q does not report the pending requirement", err)
	}
	if !strings.Contains(err.Error(), string(orchestration.PredicateRequirements)) {
		t.Fatalf("error %q does not name the failing predicate", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %q loses the deadline it wraps", err)
	}
}
