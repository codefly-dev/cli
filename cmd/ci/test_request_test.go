package ci

import "testing"

func TestTestRequestForSuitePropagatesFailFast(t *testing.T) {
	req := testRequestForSuite("integration", true)
	if req.GetSuite() != "integration" {
		t.Fatalf("suite = %q, want integration", req.GetSuite())
	}
	if !req.GetFailFast() {
		t.Fatal("CI fail-fast was not propagated to the runtime request")
	}

	defaultReq := testRequestForSuite("", false)
	if defaultReq.GetFailFast() {
		t.Fatal("fail-fast must remain disabled when the caller opts out")
	}
}
