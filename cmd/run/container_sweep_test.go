package run

import (
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestShouldSweepStaleContainersOnlyForDockerCapableSelections(t *testing.T) {
	tests := []struct {
		name    string
		runtime string
		want    bool
	}{
		{name: "free may resolve to Docker", runtime: resources.RuntimeContextFree, want: true},
		{name: "container requires Docker", runtime: resources.RuntimeContextContainer, want: true},
		{name: "native is Docker independent", runtime: resources.RuntimeContextNative},
		{name: "nix is Docker independent", runtime: resources.RuntimeContextNix},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldSweepStaleContainers(test.runtime); got != test.want {
				t.Fatalf("shouldSweepStaleContainers(%q) = %v, want %v", test.runtime, got, test.want)
			}
		})
	}
}
