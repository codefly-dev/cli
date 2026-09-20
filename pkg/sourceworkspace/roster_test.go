package sourceworkspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestSourceSelectionUsesRunningAgentsWithoutCompiledPins(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/source\n"), 0o600))
	_, err := selectPlugin(t.Context(), root)
	require.ErrorContains(t, err, "no installed compatible")

	binary := filepath.Join(t.TempDir(), "agent")
	output, err := exec.Command("go", "build", "-o", binary, "./testdata/agent").CombinedOutput()
	require.NoError(t, err, "%s", output)
	install := func(name, version string) {
		t.Helper()
		agent := &resources.Agent{Kind: resources.ServiceAgent, Publisher: "example.test", Name: name, Version: version}
		path, err := agent.Path(t.Context())
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.Symlink(binary, path))
	}
	install("unknown-source-agent", "0.0.1")
	selected, err := selectPlugin(t.Context(), root)
	require.NoError(t, err)
	require.Equal(t, "example.test/unknown-source-agent:0.0.1", selected.Identifier())
	linkedRoot := filepath.Join(t.TempDir(), "linked-source")
	require.NoError(t, os.Symlink(root, linkedRoot))
	linkedSelection, err := selectPlugin(t.Context(), linkedRoot)
	require.NoError(t, err)
	require.Equal(t, selected, linkedSelection)

	install("unknown-source-agent", "99.0.0")
	selected, err = selectPlugin(t.Context(), root)
	require.NoError(t, err)
	require.Equal(t, "99.0.0", selected.Version)

	for _, declaration := range []string{"future", "undeclared"} {
		t.Run(declaration, func(t *testing.T) {
			t.Setenv("TEST_SOURCE_CONTRACT", declaration)
			_, err := selectPlugin(t.Context(), root)
			require.ErrorContains(t, err, "no installed compatible")
		})
	}
	install("another-source-agent", "1.0.0")
	_, err = selectPlugin(t.Context(), root)
	require.ErrorContains(t, err, "multiple compatible")

	broken := &resources.Agent{Kind: resources.ServiceAgent, Publisher: "example.test", Name: "broken", Version: "1.0.0"}
	brokenPath, err := broken.Path(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(brokenPath, []byte("corrupted executable"), 0o755))
	_, err = selectPlugin(t.Context(), root)
	require.ErrorContains(t, err, "cannot discover source agents")
	require.ErrorContains(t, err, "broken")
}

func TestSourceSelectionRequiresCompleteUnambiguousLanguageSupport(t *testing.T) {
	first := Plugin{
		Agent: &resources.Agent{Publisher: "example.test", Name: "unlisted", Version: "3.1.0"},
		Info:  &agentv0.AgentInformation{Languages: []*agentv0.Language{{Type: agentv0.Language_GO}}},
	}
	for _, languages := range [][]string{nil, {"unknown"}, {"go", "python"}} {
		_, err := selectForLanguages([]Plugin{first}, languages)
		require.ErrorContains(t, err, "no installed compatible")
	}
	second := first
	second.Agent = &resources.Agent{Publisher: "other.test", Name: "unlisted", Version: "1.0.0"}
	_, err := selectForLanguages([]Plugin{first, second}, []string{"go"})
	require.ErrorContains(t, err, "multiple compatible")
	selected, err := selectForLanguages([]Plugin{first}, []string{"go"})
	require.NoError(t, err)
	selected.Version = "changed"
	require.Equal(t, "3.1.0", first.Agent.Version)
}
