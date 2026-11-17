package agents

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/sourceworkspace"
)

// An agent build says which module graph it built, in one line: the go.work
// that governs the checkout, or the pins of go.mod. The case that was silent
// is the loud one: the nearest go.work does not `use` the agent, so the build
// is pinned although a workspace sits above it.
func TestAnAgentBuildSaysWhichModuleGraphItBuilt(t *testing.T) {
	for name, tc := range map[string]struct {
		prepared   sourceworkspace.Prepared
		standalone bool
		want       string
	}{
		"a go.work governs the agent": {
			prepared: sourceworkspace.Prepared{GoWorkFile: "/tmp/x/go.work", GoWorkSource: "/repo/go.work"},
			want:     "module graph: go.work /repo/go.work",
		},
		"no go.work": {
			want: "module graph: pinned (go.mod)",
		},
		"the nearest go.work does not use the agent": {
			prepared: sourceworkspace.Prepared{GoWorkUnused: "/repo/go.work"},
			want:     "module graph: pinned (go.mod) — /repo/go.work is NOT applied: it does not `use` /repo/agents/mine; `codefly agent deps --link` adds it",
		},
		"a standalone build ignores a governing go.work, and says so": {
			prepared:   sourceworkspace.Prepared{GoWorkFile: "/tmp/x/go.work", GoWorkSource: "/repo/go.work"},
			standalone: true,
			want:       "module graph: pinned (go.mod) — standalone build, /repo/go.work is not applied",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := moduleGraphLine(&tc.prepared, "/repo/agents/mine", tc.standalone); got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
