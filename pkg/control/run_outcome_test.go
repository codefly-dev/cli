package control

import (
	"context"
	"errors"
	"testing"
)

// FlowStatus must be able to report FlowFailed/FlowStopped once a flow is no
// longer in the registry — otherwise a caller polling for readiness (e.g. the
// MCP server's run_service wait loop) can never distinguish "still starting"
// from "already failed and gone," and spins until it times out instead of
// stopping the instant the outcome is known. See recordRunOutcome/clearRunOutcome
// in impl.go and their use in Run (lifecycle.go).

func TestFlowStatusReportsRecordedFailureOnceFlowIsGone(t *testing.T) {
	plane, err := NewAt(writeWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	impl := plane.(*planeImpl)

	impl.recordRunOutcome("backend/api", FlowFailed, errors.New("boom"))

	status, err := plane.FlowStatus(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != FlowFailed {
		t.Fatalf("FlowStatus().State = %q, want %q", status.State, FlowFailed)
	}
	if status.Error != "boom" {
		t.Fatalf("FlowStatus().Error = %q, want %q", status.Error, "boom")
	}
}

func TestFlowStatusReportsRecordedStop(t *testing.T) {
	plane, err := NewAt(writeWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	impl := plane.(*planeImpl)

	impl.recordRunOutcome("backend/api", FlowStopped, nil)

	status, err := plane.FlowStatus(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != FlowStopped {
		t.Fatalf("FlowStatus().State = %q, want %q", status.State, FlowStopped)
	}
	if status.Error != "" {
		t.Fatalf("FlowStatus().Error = %q, want empty", status.Error)
	}
}

func TestFlowStatusFallsBackToIdleWithoutARecordedOutcome(t *testing.T) {
	plane, err := NewAt(writeWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	status, err := plane.FlowStatus(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != FlowIdle {
		t.Fatalf("FlowStatus().State = %q, want %q", status.State, FlowIdle)
	}
}

// FlowStatus/Stop must be addressable by flow ID: FlowManager.Active() only
// ever tracks the most-recently-registered flow, so a caller that starts two
// flows (run_service supports this via wait=false) can otherwise observe or
// stop the wrong one with no error or warning. See FlowStatus's flowID
// parameter (plane.go) and Stop's req.FlowID (types.go).

func TestFlowStatusWithExplicitFlowIDDoesNotLeakADifferentFlowsOutcome(t *testing.T) {
	plane, err := NewAt(writeWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	impl := plane.(*planeImpl)

	impl.recordRunOutcome("backend/api", FlowFailed, errors.New("api broke"))

	// A caller asking about a different, unrelated flow ID must not be told
	// about "backend/api"'s failure — it should see idle, since nothing by
	// that ID has ever run.
	status, err := plane.FlowStatus(context.Background(), "backend/worker")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != FlowIdle {
		t.Fatalf("FlowStatus(%q).State = %q, want %q (recorded outcome belongs to a different flow id)", "backend/worker", status.State, FlowIdle)
	}

	// The caller that actually started "backend/api" must still see it.
	status, err = plane.FlowStatus(context.Background(), "backend/api")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != FlowFailed {
		t.Fatalf("FlowStatus(%q).State = %q, want %q", "backend/api", status.State, FlowFailed)
	}
}

func TestStopWithUnknownFlowIDIsANoop(t *testing.T) {
	plane, err := NewAt(writeWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	// A non-empty, unregistered FlowID must be treated as "nothing to stop"
	// rather than falling back to Active() (which would stop whatever
	// unrelated flow happens to be running instead of reporting the ID was
	// wrong).
	stopped, err := plane.Stop(context.Background(), StopRequest{FlowID: "no-such-flow"})
	if err != nil {
		t.Fatalf("Stop with unknown flow id = %v, want nil", err)
	}
	if stopped {
		t.Fatal("Stop with unknown flow id reported stopped = true")
	}
}

func TestClearRunOutcomeDropsAStaleFailure(t *testing.T) {
	plane, err := NewAt(writeWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	impl := plane.(*planeImpl)

	impl.recordRunOutcome("backend/api", FlowFailed, errors.New("stale"))
	impl.clearRunOutcome()

	status, err := plane.FlowStatus(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != FlowIdle {
		t.Fatalf("FlowStatus().State after clearRunOutcome = %q, want %q", status.State, FlowIdle)
	}
}
