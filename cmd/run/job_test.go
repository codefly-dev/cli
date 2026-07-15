package run

import "testing"

func TestJobCommandReturnsErrorsThroughCobra(t *testing.T) {
	if JobCmd.RunE == nil {
		t.Fatal("job command has no RunE handler")
	}
	if JobCmd.Run != nil {
		t.Fatal("job command still has a Run handler")
	}
}
