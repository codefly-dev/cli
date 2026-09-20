package gateway

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestCodeUnitSelectionUsesDeclaredIdentityWithoutDiscovery(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	root := t.TempDir()
	writeCodeUnitFixture(t, root, "mind.yaml", "source_agents:\n  .: example.test/unknown:0.0.1\n")
	server, err := NewServer(Config{WorkDir: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	_, err = server.serviceBehaviorForCodeUnit(normalizedCodeUnitTarget{id: "root", path: ".", root: root}, "")
	require.NoError(t, err)
	require.Contains(t, server.codeUnitServices, "root\x00.\x00example.test/unknown:0.0.1")
}

func TestCodeUnitSelectionRejectsUnreachableConfigurationKeys(t *testing.T) {
	for _, key := range []string{"../escape", "./nested", "nested/../other"} {
		t.Run(key, func(t *testing.T) {
			root := t.TempDir()
			writeCodeUnitFixture(t, root, "mind.yaml", "source_agents:\n  "+key+": example.test/unknown\n")
			_, err := NewServer(Config{WorkDir: root})
			require.ErrorContains(t, err, "canonical unit paths")
		})
	}
}
