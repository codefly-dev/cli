package orchestration

import (
	"testing"
)

// readinessRequirements sits on the readiness poll, which both the TUI and the
// headless runner tick every 150ms, and multi-root turned its single OrderTo
// into one per root. These pin what that costs so a future change that makes
// the poll path superlinear is visible rather than inferred: on the fixture it
// is single-digit microseconds per root against a 150ms budget, so the root
// count is not what would make this path expensive.
func BenchmarkReadinessRequirementsOneRoot(b *testing.B) {
	flow, ctx := multiRootFlow(b, "lastlogin-go/backend")
	b.ResetTimer()
	for b.Loop() {
		if _, err := flow.readinessRequirements(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadinessRequirementsThreeRoots(b *testing.B) {
	flow, ctx := multiRootFlow(b, "lastlogin-go/backend", "wiki/backend", "analytics/reports")
	b.ResetTimer()
	for b.Loop() {
		if _, err := flow.readinessRequirements(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
