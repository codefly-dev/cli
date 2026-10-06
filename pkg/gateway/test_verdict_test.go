package gateway

import (
	"context"
	"strings"
	"testing"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"google.golang.org/grpc"

	"github.com/codefly-dev/cli/pkg/testrun"
)

// An agent that returns no verdict does not get one invented for it.
//
// Both shapes went through Server.Test as SUCCESS before pkg/testrun: an empty
// TestResponse reached the "no failures were counted" fallback, which is also
// what a run that never executed looks like, and an explicit UNKNOWN reached
// the same fallback by the same route. A test agent that crashes before
// reporting, or a harness whose output the agent cannot parse, is the case
// this protects — it used to deploy.
func TestServerTestRefusesAResponseWithNoVerdict(t *testing.T) {
	for _, shape := range []struct {
		name     string
		response *runtimev0.TestResponse
	}{
		{"empty response", &runtimev0.TestResponse{}},
		{
			"explicit UNKNOWN",
			&runtimev0.TestResponse{Result: &runtimev0.TestRunResult{State: runtimev0.TestRunResult_UNKNOWN}},
		},
		{
			// The deprecated status field said SUCCESS. It answered for the
			// run result on its own, so an agent setting only this was taken
			// at its word.
			"deprecated status says success, no run result",
			&runtimev0.TestResponse{Status: &runtimev0.TestStatus{State: runtimev0.TestStatus_SUCCESS}},
		},
		{
			// Counts that look clean, and no verdict. Zero failures is what a
			// run that never started also reports.
			"clean counts, no run result",
			&runtimev0.TestResponse{Counts: &runtimev0.TestCounts{Total: 12, Passed: 12}},
		},
	} {
		t.Run(shape.name, func(t *testing.T) {
			response := shape.response
			server := newTestServerWithRuntime(&mockRuntimeClient{
				testFn: func(context.Context, *runtimev0.TestRequest, ...grpc.CallOption) (*runtimev0.TestResponse, error) {
					return response, nil
				},
			})
			resp, err := server.Test(context.Background(), &gatewayv1.TestRequest{})
			if err != nil {
				t.Fatalf("Test: %v", err)
			}
			if resp.GetSuccess() {
				t.Fatal("a run with no verdict was reported as success")
			}
			if !strings.Contains(resp.GetOutput(), "no run-level test outcome") {
				t.Fatalf("the reply must say a verdict is missing rather than arrive empty and false: %q", resp.GetOutput())
			}
		})
	}
}

// And an explicit PASSED is still success, so the rule is a verdict check and
// not a blanket refusal.
func TestServerTestAcceptsAnExplicitPass(t *testing.T) {
	server := newTestServerWithRuntime(&mockRuntimeClient{
		testFn: func(context.Context, *runtimev0.TestRequest, ...grpc.CallOption) (*runtimev0.TestResponse, error) {
			return &runtimev0.TestResponse{
				Result: &runtimev0.TestRunResult{State: runtimev0.TestRunResult_PASSED},
				Counts: &runtimev0.TestCounts{Total: 2, Passed: 2},
			}, nil
		},
	})
	resp, err := server.Test(context.Background(), &gatewayv1.TestRequest{})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("an explicit PASSED was refused: %q", resp.GetOutput())
	}
}

// The aggregator keeps UNKNOWN as UNKNOWN.
//
// It used to run a response with no run result through the gateway's old
// success interpretation and record a positive answer as an explicit PASSED,
// so a unit that reported nothing entered the aggregate as a passing one and
// dominantTestState's ranking of UNKNOWN above FAILED never saw the case it
// exists for.
func TestTheAggregateKeepsAMissingVerdictMissing(t *testing.T) {
	for _, shape := range []struct {
		name     string
		response *runtimev0.TestResponse
		want     runtimev0.TestRunResult_State
	}{
		{"empty", &runtimev0.TestResponse{}, runtimev0.TestRunResult_UNKNOWN},
		{
			"deprecated success with no run result",
			&runtimev0.TestResponse{Status: &runtimev0.TestStatus{State: runtimev0.TestStatus_SUCCESS}},
			runtimev0.TestRunResult_UNKNOWN,
		},
		{
			"clean counts with no run result",
			&runtimev0.TestResponse{Counts: &runtimev0.TestCounts{Total: 9, Passed: 9}},
			runtimev0.TestRunResult_UNKNOWN,
		},
		{"no response at all", nil, runtimev0.TestRunResult_ERRORED},
		{
			"an explicit pass is preserved",
			&runtimev0.TestResponse{Result: &runtimev0.TestRunResult{State: runtimev0.TestRunResult_PASSED}},
			runtimev0.TestRunResult_PASSED,
		},
	} {
		t.Run(shape.name, func(t *testing.T) {
			if got := effectiveRuntimeTestState(shape.response); got != shape.want {
				t.Fatalf("state = %s, want %s", got, shape.want)
			}
		})
	}
	// And one unit without a verdict decides a run whose other unit failed:
	// "we do not know" outranks "it failed", because a missing verdict could
	// be anything including worse.
	dominant := dominantTestState(runtimev0.TestRunResult_FAILED, runtimev0.TestRunResult_UNKNOWN)
	if dominant != runtimev0.TestRunResult_UNKNOWN {
		t.Fatalf("dominant state = %s, want UNKNOWN", dominant)
	}
}

// The three former copies of the rule now agree by construction.
func TestTheGatewayAndTheSharedRuleAgree(t *testing.T) {
	for _, response := range []*runtimev0.TestResponse{
		nil,
		{},
		{Result: &runtimev0.TestRunResult{State: runtimev0.TestRunResult_UNKNOWN}},
		{Result: &runtimev0.TestRunResult{State: runtimev0.TestRunResult_PASSED}},
		{Result: &runtimev0.TestRunResult{State: runtimev0.TestRunResult_FAILED}},
		{Status: &runtimev0.TestStatus{State: runtimev0.TestStatus_SUCCESS}},
		{Counts: &runtimev0.TestCounts{Total: 4, Passed: 4}},
	} {
		if runtimeTestSuccess(response) != testrun.Passed(response) {
			t.Fatalf("the gateway and pkg/testrun disagree about %v", response)
		}
	}
}
