package companion

import "testing"

func TestNeedsLinuxCLIIncludesExecutionCompanion(t *testing.T) {
	if !needsLinuxCLI([]*Companion{{Name: "execution"}}) {
		t.Fatal("execution companion must embed a freshly built linux CLI")
	}
}
