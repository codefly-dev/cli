package conformance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/provider/manifest"
	"github.com/stretchr/testify/require"
)

func referenceManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "provider.codefly.yaml"))
	require.NoError(t, err)
	loaded, err := manifest.Load(raw)
	require.NoError(t, err)
	return loaded
}

// TestQualifyAdmitsDeclaredOperationsWithoutDelivery is the central
// qualification invariant: every declared operation is admitted by the real
// broker through plan validation, descriptor packaging and credential binding,
// and stops at the budget instead of reaching the provider's upstream API.
func TestQualifyAdmitsDeclaredOperationsWithoutDelivery(t *testing.T) {
	evidence, err := Qualify(context.Background(), referenceManifest(t), Declaration{
		Operations: []Operation{
			{Name: "create", Request: "account.create", Body: map[string]string{"name": "conformance"}},
			{Name: "observe", Request: "account.observe", PathParameters: map[string]string{"account_id": "acct_0001"}},
			{Name: "delete", Request: "account.delete", PathParameters: map[string]string{"account_id": "acct_0001"}},
		},
	})
	require.NoError(t, err)

	require.Equal(t, EvidenceVersion, evidence.SchemaVersion)
	require.Equal(t, "conformance", evidence.Provider)
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, evidence.ManifestDigest)
	require.Len(t, evidence.Operations, 3)

	byName := map[string]OperationEvidence{}
	for _, operation := range evidence.Operations {
		require.Regexp(t, `^sha256:[0-9a-f]{64}$`, operation.DescriptorDigest)
		require.Regexp(t, `^sha256:[0-9a-f]{64}$`, operation.RequestDigest)
		byName[operation.Name] = operation
	}
	// A mutating operation carries the read-only refusal; a read-only one is
	// admitted in a read-only context and can only carry the digest refusal.
	require.Equal(t, "descriptor-digest,read-only", byName["create"].NegativePaths)
	require.Equal(t, "descriptor-digest", byName["observe"].NegativePaths)
	require.True(t, byName["observe"].ReadOnly)

	// Qualification evidence never carries the host-owned credential.
	encoded, err := json.Marshal(evidence)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), qualificationCredential)
	assertNoPoison(t, encoded)
}

// TestQualifyFailsClosedOnAnUndeclaredRequest proves an operation naming a
// request the release does not package is a qualification failure, not a skip.
func TestQualifyFailsClosedOnAnUndeclaredRequest(t *testing.T) {
	_, err := Qualify(context.Background(), referenceManifest(t), Declaration{
		Operations: []Operation{{Name: "ghost", Request: "account.undeclared"}},
	})
	require.ErrorContains(t, err, "does not package")
	require.ErrorContains(t, err, "account.create")
}

// TestQualifyRejectsAnOriginRuleThatRefusesItsOwnDefault proves a manifest the
// broker would refuse at runtime is refused at qualification instead.
func TestQualifyRejectsAnOriginRuleThatRefusesItsOwnDefault(t *testing.T) {
	broken := referenceManifest(t)
	broken.OriginRules[0].Ports = []uint32{9443}

	_, err := Qualify(context.Background(), broken, Declaration{
		Operations: []Operation{{Name: "create", Request: "account.create"}},
	})
	require.ErrorContains(t, err, "does not admit its own default")
}
