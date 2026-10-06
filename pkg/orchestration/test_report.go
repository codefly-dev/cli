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
	// No counts means no counts. The deprecated flat fields are computed FROM
	// this tree by core, so falling back to them reported a number the
	// structured message had already declined to give.
	return 0, 0, 0, 0, 0
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
		// The structured result carries the typed terminal cause. The legacy
		// status fallback is gone with the rest of the deprecated reads: core's
		// proto computes the flat fields FROM this tree, so a cause that is
		// absent here is absent, not hiding in the old field.
		failure := resp.GetResult().GetFailure()
		if failure != nil {
			detail := failure.GetCode().String()
			if message := strings.TrimSpace(failure.GetMessage()); message != "" {
				detail += ": " + message
			}
			summary += ": " + detail
		} else if r := resp.GetResult(); r != nil && r.GetMessage() != "" {
			summary += ": " + r.GetMessage()
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
	// Only the structured result decides, and only PASSED is success. An
	// UNKNOWN or absent outcome is NOT inferred from counts: "no failures
	// recorded" is what a run that never started also looks like, and treating
	// that as success is how a broken test run reports green.
	return resp.GetResult().GetState() == runtimev0.TestRunResult_PASSED
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
