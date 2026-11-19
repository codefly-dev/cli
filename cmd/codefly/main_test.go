package main

import (
	"testing"

	companionindex "github.com/codefly-dev/core/companions/index"
)

// The released executable must carry its own selection. Current() would hide
// a missing registration here by using Core's test-only recipe fallback.
func TestCanonicalExecutableRegistersCompanionIndex(t *testing.T) {
	index := companionindex.Registered()
	if len(index) == 0 {
		t.Fatal("canonical codefly executable has no registered companion index")
	}
	if _, err := index.Version("proto"); err != nil {
		t.Fatal(err)
	}
}
