package orchestration

import (
	"fmt"
	"strings"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
)

// testCounts returns (total, passed, failed, skipped, errored), preferring the
// structured counts tree and falling back to the legacy flat fields so this
// works against agents that have not yet migrated to the structured schema.
func testCounts(resp *runtimev0.TestResponse) (total, passed, failed, skipped, errored int32) {
	if resp == nil {
		return
	}
	if c := resp.GetCounts(); c != nil {
		return c.GetTotal(), c.GetPassed(), c.GetFailed(), c.GetSkipped(), c.GetErrored()
	}
	//nolint:staticcheck // deprecated flat fields are the fallback for old agents.
	return resp.GetTestsRun(), resp.GetTestsPassed(), resp.GetTestsFailed(), resp.GetTestsSkipped(), 0
}

// summarizeTestResponse renders a single-line summary suitable for an error
// message: "3 failed, 1 errored, 42 passed (46 total)".
func summarizeTestResponse(resp *runtimev0.TestResponse) string {
	total, passed, failed, skipped, errored := testCounts(resp)
	var parts []string
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	if errored > 0 {
		parts = append(parts, fmt.Sprintf("%d errored", errored))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}
	parts = append(parts, fmt.Sprintf("%d passed", passed))
	summary := strings.Join(parts, ", ")
	if total > 0 {
		summary += fmt.Sprintf(" (%d total)", total)
	}
	if resp != nil {
		// The additive TestRunResult is preferred, but released agents may
		// still carry their typed terminal cause only on the legacy status.
		status := resp.GetStatus() //nolint:staticcheck // compatibility at the agent protocol boundary
		failure := resp.GetResult().GetFailure()
		if failure == nil {
			failure = status.GetFailure()
		}
		if failure != nil {
			detail := failure.GetCode().String()
			if message := strings.TrimSpace(failure.GetMessage()); message != "" {
				detail += ": " + message
			}
			summary += ": " + detail
		} else if r := resp.GetResult(); r != nil && r.GetMessage() != "" {
			summary += ": " + r.GetMessage()
		} else if message := strings.TrimSpace(status.GetMessage()); message != "" {
			summary += ": " + message
		}
	}
	return summary
}

// TestSucceeded reports whether the structured response represents a passing
// run. Prefers the structured result, falls back to the legacy status, and is
// conservative: a nil/unknown response is treated as NOT succeeded so a missing
// result can never be mistaken for a pass.
func TestSucceeded(resp *runtimev0.TestResponse) bool {
	if resp == nil {
		return false
	}
	if r := resp.GetResult(); r != nil && r.GetState() != runtimev0.TestRunResult_UNKNOWN {
		return r.GetState() == runtimev0.TestRunResult_PASSED
	}
	//nolint:staticcheck // deprecated status is the fallback for old agents.
	if s := resp.GetStatus(); s != nil {
		return s.GetState() == runtimev0.TestStatus_SUCCESS
	}
	// No structured result and no legacy status: only treat as success if a
	// run clearly happened with zero failures/errors.
	_, _, failed, _, errored := testCounts(resp)
	return failed == 0 && errored == 0
}

// RenderTestReport produces a human-readable multi-line report from a
// structured TestResponse: a headline count line plus, for failing runs, each
// failed/errored case with its location, failure message, and captured output.
func RenderTestReport(resp *runtimev0.TestResponse) string {
	if resp == nil {
		return "no test result was returned by the agent"
	}
	var b strings.Builder
	total, passed, failed, skipped, errored := testCounts(resp)
	fmt.Fprintf(&b, "Tests: %d passed", passed)
	if failed > 0 {
		fmt.Fprintf(&b, ", %d failed", failed)
	}
	if errored > 0 {
		fmt.Fprintf(&b, ", %d errored", errored)
	}
	if skipped > 0 {
		fmt.Fprintf(&b, ", %d skipped", skipped)
	}
	fmt.Fprintf(&b, " of %d\n", total)

	for _, line := range failingCaseLines(resp.GetSuites(), "") {
		b.WriteString(line)
	}
	return strings.TrimRight(b.String(), "\n")
}

// failingCaseLines walks the (possibly nested) suite tree and renders each
// failed/errored case. prefix accumulates the suite path for readability.
func failingCaseLines(suites []*runtimev0.TestSuite, prefix string) []string {
	var out []string
	for _, suite := range suites {
		path := suite.GetName()
		if prefix != "" {
			path = prefix + " › " + path
		}
		for _, c := range suite.GetCases() {
			state := c.GetState()
			if state != runtimev0.TestCaseState_TEST_CASE_STATE_FAILED &&
				state != runtimev0.TestCaseState_TEST_CASE_STATE_ERRORED {
				continue
			}
			name := c.GetFullName()
			if name == "" {
				name = c.GetName()
			}
			out = append(out, fmt.Sprintf("\n  ✗ %s › %s\n", path, name))
			if loc := c.GetLocation(); loc != nil && loc.GetFile() != "" {
				out = append(out, fmt.Sprintf("    at %s:%d\n", loc.GetFile(), loc.GetLine()))
			}
			if f := c.GetFailure(); f != nil {
				if msg := strings.TrimSpace(f.GetMessage()); msg != "" {
					out = append(out, "    "+indent(msg, "    ")+"\n")
				}
			}
			if co := strings.TrimSpace(c.GetCapturedOutput()); co != "" {
				out = append(out, "    --- output ---\n"+indent(co, "    ")+"\n")
			}
		}
		out = append(out, failingCaseLines(suite.GetSuites(), path)...)
	}
	return out
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if i == 0 {
			lines[i] = l
		} else {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}
