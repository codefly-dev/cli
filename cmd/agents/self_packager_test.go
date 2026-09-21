package agents

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

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
	predecessor := filepath.Join(home, "agents", "services", "example.test", "custom__1.0.0")
	require.NoError(t, os.MkdirAll(filepath.Dir(predecessor), 0755))
	require.NoError(t, os.WriteFile(predecessor, []byte("released predecessor bytes"), 0755))
	// Inherited cross settings cannot turn a native seed into a foreign executable.
	t.Setenv("GOOS", "plan9")
	t.Setenv("GOARCH", "386")
	manifest := &agentYAML{Publisher: "example.test", Kind: serviceAgentKind, Name: "custom", Version: "99.0.1",
		Source: &agentSource{Directory: ".", Agent: "self",
			Bootstrap: []string{"sh", "-c", `GOOS="$CODEFLY_AGENT_OS" GOARCH="$CODEFLY_AGENT_ARCH" go build -o "$CODEFLY_AGENT_OUTPUT" .`}},
	}
	prepared, privateHome, err := prepareAgentPackager(context.Background(), source, manifest, t.TempDir())
	require.NoError(t, err)
	defer prepared.Close()
	require.NotEqual(t, home, privateHome)
	require.Equal(t, "99.0.1", prepared.Service.Agent.Version)
	seed := filepath.Join(privateHome, "agents", "services", "example.test", "custom__99.0.1")
	output, err := exec.Command(seed).CombinedOutput()
	require.NoError(t, err, "%s seed did not run: %s", runtime.GOOS, output)
	require.Equal(t, "candidate-source", string(output))
	unchanged, err := os.ReadFile(predecessor)
	require.NoError(t, err)
	require.Equal(t, "released predecessor bytes", string(unchanged))
	_, err = os.Stat(filepath.Join(home, "agents", "services", "example.test", "custom__99.0.1"))
	require.True(t, os.IsNotExist(err), "candidate seed escaped into the shared plugin home")
}

func TestSelfPackagingRequiresAnExplicitDeclarationNotAnAgentIdentity(t *testing.T) {
	valid := agentYAML{Publisher: "example.test", Kind: serviceAgentKind, Name: "custom", Version: "99.0.1",
		Source: &agentSource{Directory: ".", Agent: "self", Bootstrap: []string{"true"}}}
	require.True(t, isSelfHostedSourcePackager(&valid))
	require.False(t, isSelfHostedSourcePackager(nil))
	require.False(t, isSelfHostedSourcePackager(&agentYAML{Publisher: "codefly.dev", Kind: serviceAgentKind, Name: "go", Version: "99.0.1"}))
	for _, version := range []string{"../escape", "latest", "v1.0.0", ""} {
		t.Run(fmt.Sprintf("version-%q", version), func(t *testing.T) {
			candidate := valid
			candidate.Version = version
			_, _, err := prepareAgentPackager(context.Background(), t.TempDir(), &candidate, t.TempDir())
			require.Error(t, err)
		})
	}
}

func TestSourceBootstrapRejectsMissingOutputAndPreservesInstalledArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent")
	require.NoError(t, os.WriteFile(path, []byte("existing"), 0o755))
	err := buildSourcePackager(t.Context(), t.TempDir(), path, []string{"true"})
	require.ErrorContains(t, err, "nonempty regular executable")
	payload, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "existing", string(payload))
}

func TestSelfPackagingRejectsIdentityPathTraversalBeforeBootstrap(t *testing.T) {
	for _, value := range []string{"../outside", "nested/name", ".", "..", "/absolute", `nested\name`} {
		for _, field := range []string{"publisher", "name"} {
			t.Run(field+"="+value, func(t *testing.T) {
				manifest := &agentYAML{Publisher: "example.test", Kind: serviceAgentKind, Name: "custom", Version: "1.0.0",
					Source: &agentSource{Directory: ".", Agent: "self", Bootstrap: []string{"must-not-execute"}},
				}
				if field == "publisher" {
					manifest.Publisher = value
				} else {
					manifest.Name = value
				}
				_, _, err := prepareAgentPackager(t.Context(), t.TempDir(), manifest, t.TempDir())
				require.ErrorContains(t, err, "single path components")
			})
		}
	}
}
