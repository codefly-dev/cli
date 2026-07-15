package web

import "testing"

func TestLoopbackEndpointDoesNotExposeCLIServer(t *testing.T) {
	if got, want := loopbackEndpoint(uint16(10000)), "127.0.0.1:10000"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
