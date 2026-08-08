package orchestration

import (
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
)

func TestSummarizeTestResponsePreservesTypedTerminalFailure(t *testing.T) {
	response := &runtimev0.TestResponse{Status: &runtimev0.TestStatus{
		State:   runtimev0.TestStatus_ERROR,
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
