package control

import (
	"context"
	"testing"
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
	// No flow is registered in orchestration.CurrentFlow() in a fresh test
	// process, so Stop must succeed as a no-op.
	if err := New().Stop(context.Background(), StopRequest{}); err != nil {
		t.Fatalf("Stop with nothing running = %v, want nil", err)
	}
}
