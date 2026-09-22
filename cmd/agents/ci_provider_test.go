package agents

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/provider/conformance"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The provider under test is the CLI's own normative reference manifest: a
// host-boundary fixture, not a released provider, so this gate owns the
// boundary and depends on no published artifact.
func referenceProviderManifest(t *testing.T) string {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("..", "..", "pkg", "provider", "conformance", "testdata", "provider.codefly.yaml"))
	require.NoError(t, err)
	return string(payload)
}

const providerFixtureOperations = `operations:
  - name: create
    request: account.create
    body:
      name: agent-ci
  - name: observe
    request: account.observe
    path_parameters:
      account_id: acct_0001
`

// installProviderFixture stages an agent repository whose built artifact is a
// test-only provider peer, installed into an isolated Codefly home exactly
// where agent CI's build stage installs a candidate.
func installProviderFixture(t *testing.T, agentDir string) {
	t.Helper()
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	identity := &resources.Agent{
		Kind:      resources.ProviderAgent,
		Publisher: "codefly.dev",
		Name:      "conformance",
		Version:   "1.0.0",
	}
	target, err := identity.Path(context.Background())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	build := exec.Command("go", "build", "-o", target, "./testdata/providerfixture")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "build provider fixture: %s", output)
}

func stageProviderCandidate(t *testing.T, operations string) (string, *agentYAML) {
	t.Helper()
	agentDir := t.TempDir()
	writeFile(t, filepath.Join(agentDir, providerManifestName), referenceProviderManifest(t))
	require.NoError(t, os.MkdirAll(filepath.Join(agentDir, "conformance"), 0o755))
	writeFile(t, filepath.Join(agentDir, "conformance", "operations.yaml"), operations)
	installProviderFixture(t, agentDir)
	candidate := &agentYAML{
		Publisher:   "codefly.dev",
		Kind:        string(resources.ProviderAgent),
		Name:        "conformance",
		Version:     "1.0.0",
		Conformance: &agentConformance{Mode: conformanceModeProviderRequests, Fixture: "conformance/operations.yaml"},
	}
	return agentDir, candidate
}

// TestProviderConformanceQualifiesDeclaredOperations proves the shipped
// manifest is admitted by the host parser and every owner-declared operation
// is composed into the request the host would plan and admitted by the broker.
func TestProviderConformanceQualifiesDeclaredOperations(t *testing.T) {
	agentDir, candidate := stageProviderCandidate(t, providerFixtureOperations)

	payload, conformanceDir, err := runProviderConformance(context.Background(), t.TempDir(), agentDir, candidate)
	require.NoError(t, err)

	var evidence conformance.Evidence
	require.NoError(t, json.Unmarshal(payload, &evidence))
	require.Equal(t, "conformance", evidence.Provider)
	require.Len(t, evidence.Operations, 2)
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, evidence.ManifestDigest)
	// The catalog digest can only come from the started provider process.
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, evidence.CatalogDigest)

	persisted, err := os.ReadFile(filepath.Join(conformanceDir, agentCIReportFilename))
	require.NoError(t, err)
	require.JSONEq(t, string(payload), string(persisted))
}

// TestProviderConformanceRejectsABinaryThatDoesNotImplementItsManifest is why
// the stage starts the artifact at all: a manifest alone cannot show that the
// shipped binary serves the contract it was reviewed on. The fixture advertises
// a resource action the manifest does not package, which qualification refuses.
func TestProviderConformanceRejectsABinaryThatDoesNotImplementItsManifest(t *testing.T) {
	agentDir, candidate := stageProviderCandidate(t, providerFixtureOperations)
	t.Setenv("CODEFLY_PROVIDER_FIXTURE_UNDECLARED_RESOURCE", "1")

	_, _, err := runProviderConformance(context.Background(), t.TempDir(), agentDir, candidate)
	require.ErrorContains(t, err, "does not implement its reviewed manifest")
	require.ErrorContains(t, err, "undeclared")
}

// TestProviderConformanceRejectsAManifestTargetingAnotherRelease proves
// qualification stays on the artifact agent CI just built.
func TestProviderConformanceRejectsAManifestTargetingAnotherRelease(t *testing.T) {
	agentDir, candidate := stageProviderCandidate(t, providerFixtureOperations)
	candidate.Version = "9.9.9"

	_, _, err := runProviderConformance(context.Background(), t.TempDir(), agentDir, candidate)
	require.ErrorContains(t, err, "but the candidate is")
}

// TestProviderConformanceFailsClosedWithoutADeclaration proves a release that
// declares no operation fails qualification instead of passing vacuously.
func TestProviderConformanceFailsClosedWithoutADeclaration(t *testing.T) {
	for _, test := range []struct{ name, declaration, want string }{
		{"absent", "", "read provider conformance fixture"},
		{"empty", "operations: []\n", "declares no operation"},
		{"unnamed", "operations:\n  - request: account.create\n", "requires a name and a request"},
		{"no request", "operations:\n  - name: create\n", "requires a name and a request"},
		{"repeated", "operations:\n  - name: a\n    request: account.create\n  - name: a\n    request: account.observe\n", "repeats operation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if test.declaration != "" {
				writeFile(t, filepath.Join(dir, "operations.yaml"), test.declaration)
			}
			_, err := loadProviderConformanceFixture(dir, "operations.yaml")
			require.ErrorContains(t, err, test.want)
		})
	}
}

// TestProviderConformanceFailsOnAnUndeclaredRequest proves an operation naming
// a request the release does not package is a failure, not a skip.
func TestProviderConformanceFailsOnAnUndeclaredRequest(t *testing.T) {
	agentDir, candidate := stageProviderCandidate(t, "operations:\n  - name: ghost\n    request: account.undeclared\n")

	_, _, err := runProviderConformance(context.Background(), t.TempDir(), agentDir, candidate)
	require.ErrorContains(t, err, "does not package")
}
