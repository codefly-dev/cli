package companions

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	companionindex "github.com/codefly-dev/core/companions/index"
)

// TestTheIndexNamesEveryCompanionCoreBuilds: the CLI's index carries one
// version for each recipe in the core it builds with, so no companion call
// fails for want of one. The versions are the CLI's choice and need not equal
// the recipes'.
func TestTheIndexNamesEveryCompanionCoreBuilds(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/codefly-dev/core").Output()
	if err != nil {
		t.Fatalf("locate core's source: %v", err)
	}
	recipes, err := companionindex.FromRecipes(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	carried := companionindex.Registered()
	for name := range recipes {
		if carried[name] == "" {
			t.Errorf("the CLI's companion index names no version for %q", name)
		}
	}
	if _, err := os.Stat(filepath.Join("index.codefly.yaml")); err != nil {
		t.Fatal(err)
	}
}
