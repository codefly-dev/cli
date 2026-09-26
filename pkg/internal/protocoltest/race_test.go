package protocoltest

import (
	"slices"
	"testing"
)

func TestPeerBuildRacesExactlyWhenTheTestBinaryDoes(t *testing.T) {
	args := peerBuildArgs("/tmp/peer")
	if got := slices.Contains(args, "-race"); got != raceEnabled {
		t.Fatalf("peer build %v races = %v, test binary races = %v", args, got, raceEnabled)
	}
	if args[0] != "build" || !slices.Equal(args[len(args)-3:], []string{"-o", "/tmp/peer", "./testdata/peer"}) {
		t.Fatalf("peer build args = %v", args)
	}
}
