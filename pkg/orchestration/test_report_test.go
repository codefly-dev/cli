package orchestration

import (
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
)

func TestSummarizeTestResponsePreservesTypedTerminalFailure(t *testing.T) {
	// The typed cause travels on the STRUCTURED result. core's proto computes
	// the deprecated flat fields from this tree, so an agent that reports a
	// terminal failure reports it here; the legacy fallback this test used to
	// exercise is deleted rather than kept as a second place to look.
	response := &runtimev0.TestResponse{Result: &runtimev0.TestRunResult{
		State:   runtimev0.TestRunResult_ERRORED,
		Message: "test not available: generic agent has no language knowledge",
		Failure: &basev0.Failure{
			Code:      basev0.FailureCode_FAILURE_CODE_UNSUPPORTED_OPERATION,
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
