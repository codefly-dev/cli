package engine

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	workcontext "github.com/codefly-dev/sdk-go/workcontext"
)

// Acquiring a service agent refuses a governed request, before it does
// anything at all.
//
// Starting a process is an effect, and this is the one place the Gateway starts
// one. r16 executed the hole it closes: governed Build was refused at the
// transport boundary while governed ListAllCommands spawned a language agent,
// because the metadata-reading RPCs were classed as observations and reach
// here.
//
// The refusal is the FIRST statement of acquire, which is what this test pins:
// a zero ServiceTarget would fail descriptor resolution with a different error
// entirely, so a refusal arriving for this target proves nothing was resolved,
// loaded or spawned on the way to it. That also makes the test independent of
// whether any agent is installed, which a gateway-level version was not — it
// had to skip wherever nothing could start, which is most environments.
func TestAcquireRefusesAGovernedRequestBeforeStartingAnything(t *testing.T) {
	supervisor := &AgentSupervisor{}
	before := AgentStartAttempts()

	session, err := supervisor.acquire(governedContext(t), ServiceTarget{})
	if session != nil {
		t.Fatal("a governed request was given an agent session")
	}
	if err == nil {
		t.Fatal("a governed request was not refused")
	}
	if !strings.Contains(err.Error(), "requires a verified Work Context") {
		t.Fatalf("the refusal must be the governed one, not an incidental failure: %v", err)
	}
	if AgentStartAttempts() != before+1 {
		t.Fatalf("the attempt counter must record the entry: before=%d after=%d", before, AgentStartAttempts())
	}
}

// An ungoverned request is not refused HERE. It fails later, for its own
// reasons, which is the point: the guard is about the capability and not about
// the target.
func TestAcquireDoesNotRefuseAnUngovernedRequest(t *testing.T) {
	supervisor := &AgentSupervisor{}
	_, err := supervisor.acquire(context.Background(), ServiceTarget{})
	if err == nil {
		t.Skip("a zero target resolved, so there is nothing to distinguish here")
	}
	if strings.Contains(err.Error(), "requires a verified Work Context") {
		t.Fatalf("an ungoverned request was refused as governed: %v", err)
	}
}

// governedContext presents a capability carrier the way a gRPC request does.
// The carrier's content is irrelevant to this guard — effect.Carried asks only
// whether one is present — so a syntactically valid token is enough and this
// test does not need core's conformance kit.
func governedContext(t *testing.T) context.Context {
	t.Helper()
	return metadata.NewIncomingContext(
		context.Background(),
		metadata.Pairs(workcontext.HeaderName, "present"),
	)
}
