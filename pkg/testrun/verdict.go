// Package testrun holds the one rule that decides whether a test run passed.
//
// It exists because there were three. Orchestration refused anything that was
// not an explicit PASSED; the gateway's response builder fell back to the
// deprecated status field and then to "no failures were counted", so an agent
// returning an empty TestResponse reported SUCCESS; and the code-unit
// aggregator went further and rewrote that fallback's answer into an explicit
// PASSED, so a run with no outcome became a run with a passing one.
//
// A verdict is the one thing a test run produces, and a caller that cannot get
// it must not infer it. An agent that reports nothing has reported nothing:
// zero counted failures is what a run that never executed also looks like.
package testrun

import (
	"fmt"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
)

// Passed reports whether the agent returned an explicit PASSED run outcome.
//
// Nothing else is success. Not an absent outcome, not UNKNOWN, not a
// deprecated status field, and not an empty failure list.
func Passed(resp *runtimev0.TestResponse) bool {
	return resp.GetResult().GetState() == runtimev0.TestRunResult_PASSED
}

// State is the run's outcome as the agent reported it, with one substitution:
// no response at all is ERRORED, because a missing response is a failed
// invocation rather than an absent verdict.
//
// UNKNOWN is returned as UNKNOWN. It is neither promoted to PASSED nor
// demoted to FAILED: an aggregate that cannot say what happened must say
// that, and callers rank UNKNOWN above FAILED so one unit without a verdict
// decides the whole run.
func State(resp *runtimev0.TestResponse) runtimev0.TestRunResult_State {
	if resp == nil {
		return runtimev0.TestRunResult_ERRORED
	}
	return resp.GetResult().GetState()
}

// RefuseUnlessPassed turns the verdict into an error, naming what was run.
func RefuseUnlessPassed(resp *runtimev0.TestResponse, unique string, summarize func(*runtimev0.TestResponse) string) error {
	switch state := State(resp); state {
	case runtimev0.TestRunResult_PASSED:
		return nil
	case runtimev0.TestRunResult_UNKNOWN:
		return fmt.Errorf(
			"tests for %s returned no run-level outcome: the runtime agent reported %q, which is not a verdict this run may treat as success",
			unique, state)
	default:
		return fmt.Errorf("tests failed for %s: %s", unique, summarize(resp))
	}
}

// NoVerdictMessage is what a response with no outcome says for itself, so a
// gateway reply carries the reason rather than an empty output and
// Success:false.
const NoVerdictMessage = "the runtime agent returned no run-level test outcome (UNKNOWN), which is not a verdict that may be treated as success"
