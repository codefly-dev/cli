package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// A missing file is reported with its name and what needed it: the bare
// system error at the bottom of the chain says neither.
func TestActionableRootKeepsTheFileOfASystemError(t *testing.T) {
	_, err := os.Open("no-such-dir/module.contract.codefly.yaml")
	wrapped := fmt.Errorf("cannot derive the runnables of module shop: %w",
		fmt.Errorf("runnable shop/connect/ApplyOperations needs its owner's module contract: %w", err))
	got := actionableRoot(wrapped)
	for _, want := range []string{"needs its owner's module contract", "module.contract.codefly.yaml", "no such file or directory"} {
		if !strings.Contains(got, want) {
			t.Errorf("root %q misses %q", got, want)
		}
	}
	if strings.Contains(got, "cannot derive the runnables") {
		t.Errorf("the outer layers stay out of the one-line cause: %q", got)
	}
	// With nothing above it, the path error itself.
	if got := actionableRoot(err); !strings.Contains(got, "module.contract.codefly.yaml") {
		t.Errorf("a path error alone keeps its path: %q", got)
	}
}
