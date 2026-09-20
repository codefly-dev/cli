//go:build integration

package sourceworkspace_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/sourceworkspace"
	"github.com/codefly-dev/core/agents/manager"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

// This exercises an explicitly selected published artifact, not a CLI-owned
// compatibility pin. Admission still depends on its live protocol declaration.
func TestSelectedAgentAnswersSourceContractsForAGoWorkCheckout(t *testing.T) {
	selection := os.Getenv("CODEFLY_SOURCE_QUALIFY_AGENT")
	require.NotEmpty(t, selection, "set CODEFLY_SOURCE_QUALIFY_AGENT to the artifact to qualify")

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()

	root := t.TempDir()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(root, "home"))
	// An inherited local source must not substitute a cached development build.
	t.Setenv(manager.AgentSourceEnv, "")
	defer services.ClearAgents()

	module := filepath.Join(root, "module")
	require.NoError(t, os.MkdirAll(module, 0o755))
	// A governing go.work one level above the module is the shape Prepare has to
	// normalize, and the shape a promotion fixture usually lacks.
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.27.0\n\nuse ./module\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(module, "go.mod"), []byte("module fixture\n\ngo 1.27.0\n"), 0o644))
	// Deliberately dependency-free: a third-party requirement would put module
	// proxy availability in the path of a release gate.
	require.NoError(t, os.WriteFile(filepath.Join(module, "fixture.go"), []byte("package fixture\n\nfunc Sum(values []int) int {\n\ttotal := 0\n\tfor _, value := range values {\n\t\ttotal += value\n\t}\n\treturn total\n}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(module, "fixture_test.go"), []byte("package fixture\n\nimport \"testing\"\n\nfunc TestSum(t *testing.T) {\n\tif got := Sum([]int{1, 2, 3}); got != 6 {\n\t\tt.Fatalf(\"Sum = %d, want 6\", got)\n\t}\n}\n"), 0o644))

	selected, err := resources.ParseAgent(ctx, resources.ServiceAgent, selection)
	require.NoError(t, err)
	_, err = manager.ResolveLatest(ctx, selected)
	require.NoError(t, err)
	t.Logf("qualifying published artifact %s", selected.Identifier())
	prepared, err := sourceworkspace.PrepareWithAgent(ctx, module, selected)
	require.NoError(t, err)
	defer prepared.Close()

	require.Equal(t, selected.Version, prepared.Service.Agent.Version)
	require.NotEmpty(t, prepared.GoWorkFile, "governing go.work was not normalized into the ephemeral workspace")
	require.Equal(t, true, prepared.Service.Spec["with-workspace"], "normalized workspace was not advertised to the agent")

	instance, err := services.Load(ctx, prepared.Workspace, prepared.Module, prepared.Service)
	require.NoError(t, err)
	require.NoError(t, instance.LoadBuilder(ctx))
	_, err = instance.Builder.Load(ctx)
	require.NoError(t, err)

	source, err := instance.Builder.SBOM(ctx, &builderv0.SBOMRequest{})
	require.NoError(t, err, "selected agent failed the source-scope Builder.SBOM contract")
	require.Equal(t, builderv0.SBOMStatus_COMPLETE, source.GetState().GetState(), source.GetState().GetMessage())
	require.NotNil(t, source.GetBom(), "source SBOM carried no inventory")
	require.NotEmpty(t, source.GetSha256(), "source SBOM carried no deterministic digest")
	require.NotEmpty(t, source.GetTool(), "source SBOM named no resolver")

	// A tag is mutable, so a tag-only subject cannot bind evidence to the bytes
	// it describes. Accepting one would produce image evidence that silently
	// attests to whatever the tag later points at.
	image, err := instance.Builder.SBOM(ctx, &builderv0.SBOMRequest{
		Scope: builderv0.SBOMScope_SBOM_SCOPE_IMAGE,
		Subjects: []*builderv0.ImageSubject{{
			Reference: "example.invalid/fixture:latest",
			Platform:  "linux/amd64",
			Role:      "runtime",
		}},
	})
	if err == nil {
		require.NotEqual(t, builderv0.SBOMStatus_COMPLETE, image.GetState().GetState(),
			"selected agent accepted a tag-only image subject and reported complete evidence")
		require.Empty(t, image.GetImages(), "refused image request still returned inventories")
	}

	// A refusal and a crashed agent are indistinguishable above: an RPC error of
	// any cause satisfies that branch vacuously. Re-running the contract that
	// already passed proves the agent rejected the subject and kept serving,
	// rather than dying on it.
	live, err := instance.Builder.SBOM(ctx, &builderv0.SBOMRequest{})
	require.NoError(t, err, "selected agent stopped serving after the tag-only image subject")
	require.Equal(t, builderv0.SBOMStatus_COMPLETE, live.GetState().GetState(), live.GetState().GetMessage())

	packaged, err := instance.Builder.Package(ctx, &builderv0.PackageRequest{
		OutputDirectory: filepath.Join(root, "package"),
		ArtifactName:    "fixture",
	})
	require.NoError(t, err, "selected agent failed the Builder.Package contract used by codefly agent build")
	require.NotNil(t, packaged)
}
