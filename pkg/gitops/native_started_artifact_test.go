package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/contract"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

// onPythonAgent moves a fixture service off the Go family. The whole point of
// these cases is a service whose agent lays its workload out some other way,
// so the replacement is asserted rather than assumed: a fixture that quietly
// stopped naming go-grpc would otherwise leave every case below testing Go.
func onPythonAgent(t *testing.T, service string) string {
	t.Helper()
	const goAgent = "name: go-grpc\n  version: 0.0.1\n  publisher: codefly.ai"
	require.Contains(t, service, goAgent, "the shared service fixture no longer names the Go agent")
	return strings.Replace(service, goAgent, "name: python-fastapi\n  version: 0.0.112\n  publisher: codefly.dev", 1)
}

// startedPythonService lays down what a python agent starts: the interpreter
// it ran, and the sources it handed that interpreter.
func startedPythonService(t *testing.T, dir string) NativeArtifact {
	t.Helper()
	files := map[string]string{
		filepath.Join(".venv", "bin", "python"): "cpython 3.13",
		filepath.Join("src", "main.py"):         "from fastapi import FastAPI\napp = FastAPI()\n",
		filepath.Join("src", "routes.py"):       "def health():\n    return {}\n",
		"uv.lock":                               "version = 1\n",
	}
	for relative, content := range files {
		full := filepath.Join(dir, relative)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	return NativeArtifact{
		Executable: filepath.Join(dir, ".venv", "bin", "python"),
		Sources: []string{
			filepath.Join(dir, "src", "main.py"),
			filepath.Join(dir, "src", "routes.py"),
			filepath.Join(dir, "uv.lock"),
		},
	}
}

// A compiled runtime starts its own build, so the build stays the SHA-256 of
// that one executable — the same value a Go workload was approved under before
// the agent reported anything, so adopting the contract does not re-approve
// every build in the host's envelope.
func TestTheBuildOfACompiledRuntimeIsTheExecutableItStarted(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main")
	require.NoError(t, os.WriteFile(path, []byte("one build"), 0o700))

	first, err := NativeArtifact{Executable: path}.Digest(root)
	require.NoError(t, err)
	require.Equal(t, solutionhost.ImageDigest("sha256:35b5c9836731bd31e75d30f3186350b17b62adf380da9d852d822b741f05922a"), first)

	require.NoError(t, os.WriteFile(path, []byte("another build"), 0o700))
	second, err := NativeArtifact{Executable: path}.Digest(root)
	require.NoError(t, err)
	require.NotEqual(t, first, second, "a rebuild is a different build")

	_, err = NativeArtifact{Executable: filepath.Join(t.TempDir(), "absent")}.Digest(root)
	require.Error(t, err)

	_, err = NativeArtifact{}.Digest(root)
	require.ErrorContains(t, err, "no started executable")
}

// An interpreted runtime starts an interpreter, which says nothing about the
// service: the build is the closure the agent named, and every file in it is
// part of what the host approves.
func TestTheBuildOfAnInterpretedRuntimeIsTheClosureTheAgentReported(t *testing.T) {
	root := t.TempDir()
	artifact := startedPythonService(t, root)

	digest, err := artifact.Digest(root)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(digest), "sha256:"))

	interpreter, err := NativeArtifact{Executable: artifact.Executable}.Digest(root)
	require.NoError(t, err)
	require.NotEqual(t, interpreter, digest, "the interpreter alone is not the build: every python service would share one")

	// The agent's enumeration order is not part of the build, and naming a
	// file twice is not a different build either.
	shuffled := NativeArtifact{Executable: artifact.Executable, Sources: []string{
		artifact.Sources[2], artifact.Sources[1], artifact.Sources[0], artifact.Sources[1],
	}}
	same, err := shuffled.Digest(root)
	require.NoError(t, err)
	require.Equal(t, digest, same)

	// Editing any source is a different build — including the dependency lock,
	// which is as much what runs as the code importing it.
	for _, source := range artifact.Sources {
		original, readErr := os.ReadFile(source) //nolint:gosec // G304: a path this test wrote under its own TempDir
		require.NoError(t, readErr)
		require.NoError(t, os.WriteFile(source, append(original, []byte("# edited\n")...), 0o600))
		edited, digestErr := artifact.Digest(root)
		require.NoError(t, digestErr)
		require.NotEqual(t, digest, edited, "%s changed and the build did not", source)
		require.NoError(t, os.WriteFile(source, original, 0o600))
	}
	restored, err := artifact.Digest(root)
	require.NoError(t, err)
	require.Equal(t, digest, restored, "the same tree is the same build")

	// A file the agent names is a file: a directory is the agent declining to
	// say what its closure contains, which is not the run's to decide.
	_, err = NativeArtifact{Executable: artifact.Executable, Sources: []string{filepath.Join(root, "src")}}.Digest(root)
	require.ErrorContains(t, err, "not a regular file")
}

// The closure is named relative to the workspace, so the same checkout in a
// different directory is the same build and the host's envelope does not grow
// a second approval for it.
func TestAClosureDigestDoesNotMoveWithTheCheckout(t *testing.T) {
	first := t.TempDir()
	here, err := startedPythonService(t, first).Digest(first)
	require.NoError(t, err)

	second := t.TempDir()
	there, err := startedPythonService(t, second).Digest(second)
	require.NoError(t, err)
	require.Equal(t, here, there)
}

// An agent that reports nothing is where a native run stops. It must say which
// agent that is: adopting the contract there is the only thing that lifts the
// refusal, and the old message named a Go binary path the service never had.
func TestAnUnreportedArtifactIsRefusedByNamingTheAgent(t *testing.T) {
	workspace, _ := writeAuthorityWorkspaceRunning(t, onPythonAgent(t, exposedHostedServiceYAML("api", "shop"))+"module-identity: true\n")
	service := loadServices(t, workspace, "shop", "api")[0]

	_, err := StartedArtifactOf(service, nil)
	require.ErrorContains(t, err, "codefly.dev/python-fastapi:0.0.112")
	require.ErrorContains(t, err, contract.RuntimeStartedArtifact)
	require.ErrorContains(t, err, "shop/api")
	require.NotContains(t, err.Error(), ".cache", "the refusal must not name a path from one agent family's layout")

	// An artifact naming no program names nothing, however many sources it
	// lists: the same refusal, not an empty digest.
	_, err = StartedArtifactOf(service, &runtimev0.StartedArtifact{Sources: []string{"/workspace/src/main.py"}})
	require.ErrorContains(t, err, "codefly.dev/python-fastapi:0.0.112")

	reported, err := StartedArtifactOf(service, &runtimev0.StartedArtifact{
		Executable: "/opt/venv/bin/python", Sources: []string{"/workspace/src/main.py"},
	})
	require.NoError(t, err)
	require.Equal(t, NativeArtifact{Executable: "/opt/venv/bin/python", Sources: []string{"/workspace/src/main.py"}}, reported)
}

// The delivery of a module-identity service on a non-Go agent: its presence
// names the build the agent reported starting, and its authority approves that
// same build. Nothing reads a path of the run's own choosing — the service has
// no `.cache/native/main`, which is exactly what used to stop it being
// delivered at all (#989).
func TestANativeRunDeliversAServiceOnANonGoAgent(t *testing.T) {
	ctx := context.Background()
	workspace, module := writeAuthorityWorkspaceRunning(t, onPythonAgent(t, exposedHostedServiceYAML("api", "shop"))+"module-identity: true\n")
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")

	artifact := startedPythonService(t, services[0].Dir())
	require.NoFileExists(t, filepath.Join(services[0].Dir(), ".cache", "native", "main"))

	deliverer := &NativeDeliverer{Workspace: workspace, Env: env, StateDir: t.TempDir()}
	documents, err := deliverer.Render(ctx, &NativeWorkload{
		Module: module, Service: services[0], Services: services,
		Namespace: "acme-shop", Account: "api", SPIFFEID: "spiffe://cluster.example/ns/acme-shop/sa/api",
		Subject: "shop-api@example.iam.test", Artifact: artifact,
	})
	require.NoError(t, err)

	expected, err := artifact.Digest(workspace.Dir())
	require.NoError(t, err)
	require.Equal(t, expected, documents.Digest)
	require.Len(t, documents.Presence.Workloads, 1)
	require.Equal(t, expected, documents.Presence.Workloads[0].Image.Digest)
	require.Equal(t, "native/acme/shop/api", documents.Presence.Workloads[0].Image.Repository)
	require.NotNil(t, documents.Authority)
	require.Equal(t, expected, documents.Authority.ApprovedBuild)

	// A rebuilt source is a new build the host must approve afresh, the same
	// way a recompiled Go binary is.
	require.NoError(t, os.WriteFile(artifact.Sources[0], []byte("app = None\n"), 0o600))
	rebuilt, err := deliverer.Render(ctx, &NativeWorkload{
		Module: module, Service: services[0], Services: services,
		Namespace: "acme-shop", Account: "api", SPIFFEID: "spiffe://cluster.example/ns/acme-shop/sa/api",
		Subject: "shop-api@example.iam.test", Artifact: artifact,
	})
	require.NoError(t, err)
	require.NotEqual(t, documents.Digest, rebuilt.Digest)
}
