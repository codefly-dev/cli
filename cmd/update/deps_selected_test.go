package update

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/dependencypins"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/modfile"
)

func TestSelectedDependencyUpdatesIndirectWithoutMovingOthers(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")
	root := t.TempDir()
	files := map[string]string{
		"go.mod":               "module example.test/app\ngo 1.23\nrequire (\nexample.test/direct v1.0.0\nexample.test/indirect v1.0.0 // indirect\n)\nreplace example.test/direct => ./direct\nreplace example.test/indirect => ./indirect\n",
		"app.go":               "package app\nimport \"example.test/direct\"\nvar Value = direct.Value\n",
		"direct/go.mod":        "module example.test/direct\ngo 1.23\nrequire example.test/indirect v1.0.0\nreplace example.test/indirect => ../indirect\n",
		"direct/direct.go":     "package direct\nimport \"example.test/indirect\"\nvar Value = indirect.Value\n",
		"indirect/go.mod":      "module example.test/indirect\ngo 1.23\n",
		"indirect/indirect.go": "package indirect\nconst Value = 42\n",
	}
	for name, data := range files {
		path := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(data), 0o644))
	}
	// The existing module resolver is real; local replacements keep the fixture offline.
	pins, err := dependencypins.Plan([]string{root, filepath.Join(root, "indirect")}, []string{"example.test/indirect@v1.0.1"})
	require.NoError(t, err)
	require.Empty(t, pins[filepath.Join(root, "indirect")], "do not inject a requirement into a module that never used it")
	oldDir, oldAudit, oldSelections := depsDir, depsAudit, depsSelections
	t.Cleanup(func() { depsDir, depsAudit, depsSelections = oldDir, oldAudit, oldSelections })
	depsDir, depsAudit = root, false
	depsSelections = []string{"example.test/indirect@v1.0.1"}
	ctx, done := common.NewContext()
	defer done()
	require.NoError(t, DepsCmd.RunE(DepsCmd, nil))
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err)
	parsed, err := modfile.Parse("go.mod", data, nil)
	require.NoError(t, err)
	versions := map[string]string{}
	for _, r := range parsed.Require {
		versions[r.Mod.Path] = r.Mod.Version
	}
	require.Equal(t, "v1.0.1", versions["example.test/indirect"])
	require.Equal(t, "v1.0.0", versions["example.test/direct"])
	require.NoError(t, runGo(ctx, root, "build", "./..."))
	require.NoError(t, runGo(ctx, root, "mod", "tidy", "-diff"))
	before := string(data)
	depsSelections = []string{"example.test/indirect@v1.0.2", "example.test/missing@v1.0.1"}
	err = DepsCmd.RunE(DepsCmd, nil)
	require.Error(t, err)
	data, err = os.ReadFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err)
	require.Equal(t, before, string(data), "all selections must validate before mutation")
}
