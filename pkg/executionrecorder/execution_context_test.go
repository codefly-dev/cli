package executionrecorder

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestExecutionContextRoundTripsThroughGRPCMetadata(t *testing.T) {
	execution, err := NewExecutionContext("e30.AAAA", "operation-1")
	if err != nil {
		t.Fatal(err)
	}
	outgoing, err := WithGRPCExecutionContext(t.Context(), execution)
	if err != nil {
		t.Fatal(err)
	}
	carrier, ok := metadata.FromOutgoingContext(outgoing)
	if !ok {
		t.Fatal("no outgoing metadata")
	}
	incoming := metadata.NewIncomingContext(t.Context(), carrier)
	got, present, err := ExecutionContextFromIncomingIfPresent(incoming)
	if err != nil || !present {
		t.Fatalf("present=%v err=%v", present, err)
	}
	if got.WorkContext() != "e30.AAAA" || got.OperationID() != "operation-1" {
		t.Fatalf("execution = %+v", got)
	}

	// A carrier already set is refused rather than overwritten or joined.
	if _, err := WithGRPCExecutionContext(outgoing, execution); !errors.Is(err, ErrInvalid) {
		t.Fatalf("second attachment err = %v", err)
	}
	// No carrier at all is "not present"; half a carrier is an error.
	if _, present, err := ExecutionContextFromIncomingIfPresent(t.Context()); err != nil || present {
		t.Fatalf("absent carrier: present=%v err=%v", present, err)
	}
	partial := metadata.NewIncomingContext(t.Context(), metadata.Pairs(OperationIDMetadataKey, "operation-1"))
	if _, _, err := ExecutionContextFromIncomingIfPresent(partial); !errors.Is(err, ErrInvalid) {
		t.Fatalf("partial carrier err = %v", err)
	}
	duplicate := metadata.NewIncomingContext(t.Context(), metadata.Pairs(
		WorkContextMetadataKey, "e30.AAAA", WorkContextMetadataKey, "e30.BBBB", OperationIDMetadataKey, "operation-1"))
	if _, err := ExecutionContextFromIncoming(duplicate); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate carrier err = %v", err)
	}
}

func TestExecutionContextHoldsItsBounds(t *testing.T) {
	for name, input := range map[string][2]string{
		"empty capability":   {"", "operation-1"},
		"oversized":          {strings.Repeat("a", 32<<10+1), "operation-1"},
		"empty operation":    {"e30.AAAA", ""},
		"padded operation":   {"e30.AAAA", " operation-1"},
		"operation too long": {"e30.AAAA", strings.Repeat("o", 129)},
		"operation with /":   {"e30.AAAA", "operation/1"},
	} {
		if _, err := NewExecutionContext(input[0], input[1]); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if _, err := NewExecutionContext("e30.AAAA", "op.1:a-b_c"); err != nil {
		t.Fatal(err)
	}
}
