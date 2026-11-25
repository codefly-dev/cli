package orchestration

import (
	"strings"
	"testing"

	basev1 "github.com/codefly-dev/core/generated/go/codefly/base/v1"
	runtimev1 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v1"
)

func TestSummarizeTestResponsePreservesTypedTerminalFailure(t *testing.T) {
	// The typed cause travels on the STRUCTURED result. core's proto computes
	// the deprecated flat fields from this tree, so an agent that reports a
	// terminal failure reports it here; the legacy fallback this test used to
	// exercise is deleted rather than kept as a second place to look.
	response := &runtimev1.TestResponse{Result: &runtimev1.TestRunResult{
		State:   runtimev1.TestRunResult_ERRORED,
		Message: "test not available: generic agent has no language knowledge",
		Failure: &basev1.Failure{
			Code:      basev1.FailureCode_FAILURE_CODE_UNSUPPORTED_OPERATION,
			Operation: "runtime.test",
			Message:   "test not available: generic agent has no language knowledge",
		},
	}}

	summary := summarizeTestResponse(response)
	if !strings.Contains(summary, "FAILURE_CODE_UNSUPPORTED_OPERATION") ||
		!strings.Contains(summary, "generic agent has no language knowledge") {
		t.Fatalf("summary = %q, want typed terminal failure instead of a count-only error", summary)
	}
}

// Only PASSED is success, and an absent verdict is a failure rather than a pass.
//
// The rule's whole point is the zero value: TestRunResult_UNKNOWN is 0, so an
// agent that reports no run-level outcome would read as success under any
// "not explicitly failed" test. It refuses instead.
func TestOnlyAPassedRunIsSuccess(t *testing.T) {
	for _, test := range []struct {
		name  string
		resp  *runtimev1.TestResponse
		allow bool
	}{
		{"passed", &runtimev1.TestResponse{Result: &runtimev1.TestRunResult{State: runtimev1.TestRunResult_PASSED}}, true},
		{"failed", &runtimev1.TestResponse{Result: &runtimev1.TestRunResult{State: runtimev1.TestRunResult_FAILED}}, false},
		{"errored", &runtimev1.TestResponse{Result: &runtimev1.TestRunResult{State: runtimev1.TestRunResult_ERRORED}}, false},
		{"timed out", &runtimev1.TestResponse{Result: &runtimev1.TestRunResult{State: runtimev1.TestRunResult_TIMED_OUT}}, false},
		{"unknown", &runtimev1.TestResponse{Result: &runtimev1.TestRunResult{State: runtimev1.TestRunResult_UNKNOWN}}, false},
		{"no result at all", &runtimev1.TestResponse{}, false},
		{"nothing at all", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := refuseUnlessTestRunPassed(test.resp, "mod/svc")
			if test.allow && err != nil {
				t.Fatalf("want success, got %v", err)
			}
			if !test.allow && err == nil {
				t.Fatal("want a refusal, got success")
			}
			if !test.allow && test.name == "unknown" && !strings.Contains(err.Error(), "no run-level outcome") {
				t.Fatalf("an absent verdict must say so: %v", err)
			}
		})
	}
}
