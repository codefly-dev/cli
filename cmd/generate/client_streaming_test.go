package generate

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	coreproto "github.com/codefly-dev/core/companions/proto"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Exercise the published companion through `codefly generate client`. Checking
// Core's template source alone misses a companion that still ships the old one.
func TestGenerateClientPythonStreaming(t *testing.T) {
	requireQualify(t)
	ctx := t.Context()
	root := t.TempDir()
	protoDir := filepath.Join(root, "proto")
	writeTestFile(t, filepath.Join(protoDir, "buf.yaml"), "version: v2\nmodules:\n  - path: .\n")
	fixture, err := os.ReadFile("testdata/python_streaming/shapes.proto")
	require.NoError(t, err)
	writeTestFile(t, filepath.Join(protoDir, "shapes.proto"), string(fixture))
	harness, err := os.ReadFile("testdata/python_streaming/exercise.py")
	require.NoError(t, err)
	contract, err := buildDescriptorSet(ctx, protoDir)
	require.NoError(t, err)
	var set descriptorpb.FileDescriptorSet
	require.NoError(t, googleproto.Unmarshal(contract, &set))
	contracts := writeContractsCatalog(t, root, "codefly/shapes", contract, composition.APIContractEndpoint{
		Service: "api", Endpoint: "connect", API: "connect",
		Kind: composition.APIContractKindProtobuf, Package: "shapes.v1",
		Services: composition.ProtobufServices(&set, "shapes.v1"),
	})
	workspace := &resources.Workspace{Name: "streaming-test", Layout: resources.LayoutKindModules}
	require.NoError(t, workspace.SaveToDirUnsafe(ctx, root))
	t.Chdir(root)
	resetClientFlags(t)
	clientFrom, clientLanguages = "contracts:"+contracts, []string{"python"}
	clientName, clientModuleName = "shapes-client", "shapes"
	clientOutput = filepath.Join(root, "library")
	require.NoError(t, ClientCmd.RunE(ClientCmd, nil))

	pythonDir := filepath.Join(clientOutput, "python")
	facade := filepath.Join(pythonDir, "shapes_client", "shapes.py")
	for method, seam := range map[string]string{
		"Echo": "unary", "Fan": "unary_stream", "Gather": "stream_unary", "Chat": "stream_stream",
	} {
		assertFileContains(t, facade, `self._gateway.`+seam+`("/shapes.v1.EchoService/`+method+`",`)
	}
	// Use the same generated bindings and runtime as the companion. Requests
	// travel through the generated facade, never a hand-maintained RPC wrapper.
	image, err := coreproto.CompanionImage(ctx)
	require.NoError(t, err)
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i",
		"-v", pythonDir+":/out:ro", "-w", "/out", image.FullName(),
		"/venv/bin/python", "-")
	cmd.Stdin = bytes.NewReader(harness)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	t.Logf("%s", out)
}
