package companion

import (
	"slices"
	"testing"

	"github.com/codefly-dev/core/companions"
	"github.com/stretchr/testify/require"
)

// leafCompanion stands for a companion that builds on no base and bakes a
// freshly built linux CLI itself. Core declares none today, so the tests that
// need one declare it beside core's own specs.
const leafCompanion = "leaf"

// withLeafSpec makes loadBuildSpecs answer core's specs plus leafCompanion's,
// right after the base companion it does not build on, for this test only.
func withLeafSpec(t *testing.T) {
	t.Helper()
	declared, err := companions.BuildSpecs()
	require.NoError(t, err)
	leaf := companions.BuildSpec{
		Name:       leafCompanion,
		Dockerfile: "companions/" + leafCompanion + "/Dockerfile",
		Context:    ".",
		Platforms:  []string{"linux/amd64", "linux/arm64"},
		CLIBinary:  "bin/linux/${TARGETARCH}/codefly",
	}
	at := slices.IndexFunc(declared, func(spec companions.BuildSpec) bool { return spec.Name == baseCompanionName })
	require.GreaterOrEqual(t, at, 0, "core declares no %s base companion", baseCompanionName)
	specs := slices.Insert(slices.Clone(declared), at+1, leaf)
	loaded := buildSpecs{byName: map[string]companions.BuildSpec{}, order: map[string]int{}}
	for i, spec := range specs {
		loaded.byName[spec.Name] = spec
		loaded.order[spec.Name] = i
	}
	previous := loadBuildSpecs
	loadBuildSpecs = func() (buildSpecs, error) { return loaded, nil }
	t.Cleanup(func() { loadBuildSpecs = previous })
}
