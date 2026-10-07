package effect

import (
	"context"
	"testing"
	"time"

	coreworkcontext "github.com/codefly-dev/core/workcontext"
	"google.golang.org/grpc/metadata"

	workcontext "github.com/codefly-dev/sdk-go/workcontext"
	workcontextgrpc "github.com/codefly-dev/sdk-go/workcontext/grpctransport"
)

// governedContext is a request presenting a real capability carrier.
//
// The carrier comes from core's conformance kit, not from a hand-made string:
// this boundary does not verify, but it DOES parse, and a hand-made carrier
// proves the refusal only for bytes no caller would send. The capability is
// never verified here — the refusal is the point — so an accepted fixture's
// token is exactly the right input.
func governedContext(t *testing.T) context.Context {
	t.Helper()
	return contextCarrying(t, conformanceCarrier(t))
}

func ungovernedContext() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.New(nil))
}

// contextCarrying attaches a carrier the way the SDK attaches it, so the key
// and encoding are the SDK's rather than this test's guess at them.
func contextCarrying(t *testing.T, token string) context.Context {
	t.Helper()
	// A well-formed carrier is attached the way the SDK attaches it, so the
	// key and encoding are the SDK's rather than this test's guess at them. A
	// deliberately malformed one cannot be: the SDK validates the token when
	// it attaches it, so it is attached by hand at the SDK's own key — which
	// is the case that matters, since the boundary must tell "present but
	// unreadable" from "absent".
	byHand := metadata.NewIncomingContext(
		context.Background(),
		metadata.Pairs(workcontext.HeaderName, token),
	)
	execution, err := workcontextgrpc.NewExecutionContext(token, "operation-effect-boundary")
	if err != nil {
		return byHand
	}
	outgoing, err := workcontextgrpc.WithGRPCExecutionContext(context.Background(), execution)
	if err != nil {
		return byHand
	}
	carrier, ok := metadata.FromOutgoingContext(outgoing)
	if !ok {
		t.Fatal("the SDK attached no outgoing execution metadata")
	}
	return metadata.NewIncomingContext(context.Background(), carrier.Copy())
}

func conformanceCarrier(t *testing.T) string {
	t.Helper()
	fixtures, err := coreworkcontext.Fixtures(time.Date(2026, time.July, 23, 19, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("core's conformance fixtures: %v", err)
	}
	for _, fixture := range fixtures {
		if fixture.Outcome == coreworkcontext.OutcomeAccepted && fixture.Token != "" {
			return fixture.Token
		}
	}
	t.Fatal("core's conformance kit produced no accepted capability carrier")
	return ""
}
