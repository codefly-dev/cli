package agents

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/codefly-dev/cli/pkg/sourceworkspace"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestCandidatePackagerUsesPrivateIdentityAndStandaloneHostCompiler(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	require.NoError(t, os.MkdirAll(source, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(source, "go.mod"), []byte("module example.invalid/packager\n\ngo 1.27.0\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "main.go"), []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Print(\"candidate-source\") }\n"), 0600))
	// An unrelated enclosing workspace must not govern the seed's source graph.
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.27.0\nuse ./missing-module\n"), 0600))
	home := t.TempDir()
	t.Setenv(resources.CodeflyHomeEnv, home)
	predecessor := sourcePackagerPath(home)
	require.NoError(t, os.MkdirAll(filepath.Dir(predecessor), 0755))
	require.NoError(t, os.WriteFile(predecessor, []byte("released predecessor bytes"), 0755))
	// Inherited cross settings cannot turn a native seed into a foreign executable.
	t.Setenv("GOOS", "plan9")
	t.Setenv("GOARCH", "386")
	manifest := &agentYAML{Publisher: genericGoPluginPublisher, Kind: serviceAgentKind, Name: "go", Version: "99.0.1"}
	prepared, privateHome, err := prepareAgentPackager(context.Background(), source, manifest, t.TempDir())
	require.NoError(t, err)
	defer prepared.Close()
	require.NotEqual(t, home, privateHome)
	require.Equal(t, "99.0.1", prepared.Service.Agent.Version)
	require.Equal(t, sourceworkspace.GenericGoPluginVersion, filepath.Base(predecessor)[len("go__"):])
	seed := filepath.Join(privateHome, "agents", "services", genericGoPluginPublisher, "go__99.0.1")
	output, err := exec.Command(seed).CombinedOutput()
	require.NoError(t, err, "%s seed did not run: %s", runtime.GOOS, output)
	require.Equal(t, "candidate-source", string(output))
	unchanged, err := os.ReadFile(predecessor)
	require.NoError(t, err)
	require.Equal(t, "released predecessor bytes", string(unchanged))
	_, err = os.Stat(filepath.Join(home, "agents", "services", genericGoPluginPublisher, "go__99.0.1"))
	require.True(t, os.IsNotExist(err), "candidate seed escaped into the shared plugin home")
}

func TestSelfPackagingIsLimitedToCanonicalRootSource(t *testing.T) {
	valid := agentYAML{Publisher: genericGoPluginPublisher, Kind: serviceAgentKind, Name: "go", Version: "99.0.1"}
	require.True(t, isCanonicalSourcePackager(&valid))
	require.False(t, isCanonicalSourcePackager(nil))
	for _, change := range []func(*agentYAML){
		func(m *agentYAML) { m.Publisher = "example.invalid" },
		func(m *agentYAML) { m.Kind = "codefly:module" },
		func(m *agentYAML) { m.Name = "another" },
		func(m *agentYAML) { m.Source = &agentSource{Directory: ".", Agent: "codefly.dev/go:0.0.49"} },
	} {
		candidate := valid
		change(&candidate)
		require.False(t, isCanonicalSourcePackager(&candidate))
	}
	for _, version := range []string{"../escape", "latest", "v1.0.0", ""} {
		t.Run(fmt.Sprintf("version-%q", version), func(t *testing.T) {
			candidate := valid
			candidate.Version = version
			_, _, err := prepareAgentPackager(context.Background(), t.TempDir(), &candidate, t.TempDir())
			require.Error(t, err)
		})
	}
}
