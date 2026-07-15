package endpoint

import (
	"strings"
	"testing"
)

func TestEndpointCommandReturnsInvalidAPIError(t *testing.T) {
	previous, err := Cmd.Flags().GetString("type")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Cmd.Flags().Set("type", previous) })
	if err := Cmd.Flags().Set("type", "not-an-api"); err != nil {
		t.Fatal(err)
	}

	err = Cmd.RunE(Cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "not-an-api") {
		t.Fatalf("invalid API error = %v", err)
	}
}

func TestEndpointCommandRejectsExtraArguments(t *testing.T) {
	if err := Cmd.Args(Cmd, []string{"one", "two"}); err == nil {
		t.Fatal("expected endpoint to reject more than one service argument")
	}
}
