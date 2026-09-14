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

// sourcePinQualifyEnv gates the qualification on an explicit opt-in: it
// downloads the pinned agent and runs a real toolchain, so an unqualified
// machine must fail loudly rather than skip into a false pass.
const sourcePinQualifyEnv = "CODEFLY_SOURCE_PIN_QUALIFY"

// The source-workspace roster pin is promoted through `codefly agent
// promote-source`, which qualifies a candidate only through `codefly test
// source`. The pinned agent answers two further contracts the CLI depends on
// and that gate never exercises: Builder.SBOM, which image evidence is built
// on, and Builder.Package, which `codefly agent build` resolves through this
// same pin. Real checkouts are also governed by a go.work that the ephemeral
// workspace has to normalize, while a promotion fixture is typically a bare
// module. A promotion could therefore move the pin onto a release that
// regressed any of the three and every test here would still pass.
//
// This drives the version the CLI actually embeds, so each future promotion
// re-proves them instead of resting on the promoter's choice of fixture.
func TestPinnedGoAgentAnswersSourceContractsForAGoWorkCheckout(t *testing.T) {
	require.NotEmpty(t, os.Getenv(sourcePinQualifyEnv),
		"set "+sourcePinQualifyEnv+"=1 to qualify the embedded source-workspace pin against its released agent")

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()

	root := t.TempDir()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(root, "home"))
	// The pin is exact, but an inherited "local" source would resolve it from a
	// developer's cache instead of the published artifact this asserts about.
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

	prepared, err := sourceworkspace.Prepare(ctx, module)
	require.NoError(t, err)
	defer prepared.Close()

	require.Equal(t, sourceworkspace.GenericGoPluginVersion, prepared.Service.Agent.Version,
		"qualification must run the embedded pin, not some other version")
	require.NotEmpty(t, prepared.GoWorkFile, "governing go.work was not normalized into the ephemeral workspace")
	require.Equal(t, true, prepared.Service.Spec["with-workspace"], "normalized workspace was not advertised to the agent")

	instance, err := services.Load(ctx, prepared.Workspace, prepared.Module, prepared.Service)
	require.NoError(t, err)
	require.NoError(t, instance.LoadBuilder(ctx))
	_, err = instance.Builder.Load(ctx)
	require.NoError(t, err)

	source, err := instance.Builder.SBOM(ctx, &builderv0.SBOMRequest{})
	require.NoError(t, err, "pinned agent failed the source-scope Builder.SBOM contract")
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
			"pinned agent accepted a tag-only image subject and reported complete evidence")
		require.Empty(t, image.GetImages(), "refused image request still returned inventories")
	}

	// A refusal and a crashed agent are indistinguishable above: an RPC error of
	// any cause satisfies that branch vacuously. Re-running the contract that
	// already passed proves the agent rejected the subject and kept serving,
	// rather than dying on it.
	live, err := instance.Builder.SBOM(ctx, &builderv0.SBOMRequest{})
	require.NoError(t, err, "pinned agent stopped serving after the tag-only image subject")
	require.Equal(t, builderv0.SBOMStatus_COMPLETE, live.GetState().GetState(), live.GetState().GetMessage())

	packaged, err := instance.Builder.Package(ctx, &builderv0.PackageRequest{
		OutputDirectory: filepath.Join(root, "package"),
		ArtifactName:    "fixture",
	})
	require.NoError(t, err, "pinned agent failed the Builder.Package contract that codefly agent build resolves through this pin")
	require.NotNil(t, packaged)
}
