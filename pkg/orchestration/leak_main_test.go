//go:build goleak

package orchestration_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak after the package's tests complete. Currently
// gated behind the `goleak` build tag because several existing tests
// leak goroutines (they spawn Playbook.Begin in a goroutine without
// cancelling the inner ctx — test hygiene, not production code). To
// catch new regressions:
//
//	go test -tags goleak ./cli/pkg/orchestration/...
//
// Once the existing tests are updated to cancel their contexts on
// teardown, remove the build tag so the check runs by default.
//
// The production code is already goroutine-clean: the Signaller leak
// was fixed by using a buffered+non-blocking send, the Runner.Follow
// ticker respects ctx.Done, the ActionManager send is ctx-aware.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("google.golang.org/grpc.(*ccResolverWrapper).serializer"),
		goleak.IgnoreTopFunction("go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).processQueue"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
	)
}
