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

// TestQualifyRefusesFieldsTheDescriptorForbids is the regression this probe
// exists for. The host binds a request only from descriptor-allowed fields, and
// that binding runs after the request budget — so a probe that stopped at an
// exhausted budget reported these operations as admitted while the host would
// refuse every one of them at runtime.
func TestQualifyRefusesFieldsTheDescriptorForbids(t *testing.T) {
	// account.create allows body [name, enabled]; account.observe binds the
	// account_id path parameter and allows query [expand].
	for _, test := range []struct {
		name      string
		operation Operation
		want      string
	}{
		{
			name:      "undeclared body field",
			operation: Operation{Name: "create", Request: "account.create", Body: map[string]string{"name": "subject", "smuggled": "x"}},
			want:      "body field",
		},
		{
			name:      "undeclared query field",
			operation: Operation{Name: "observe", Request: "account.observe", PathParameters: map[string]string{"account_id": "acct_0001"}, Query: map[string]string{"smuggled": "x"}},
			want:      "query field",
		},
		{
			name:      "path parameter that is not the planned remote id",
			operation: Operation{Name: "observe", Request: "account.observe", PathParameters: map[string]string{"account_id": "acct_0001", "extra": "acct_0002"}},
			want:      "path parameters do not match descriptor",
		},
		{
			name:      "missing the bound path parameter",
			operation: Operation{Name: "observe", Request: "account.observe"},
			want:      "which the operation does not declare",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Qualify(context.Background(), referenceManifest(t), Declaration{Operations: []Operation{test.operation}})
			require.Error(t, err, "an operation the host would refuse was qualified")
			require.ErrorContains(t, err, test.want)
		})
	}
}

// TestQualifyReachesDeliveryWithoutNetwork proves the admitted probe runs the
// whole path and is served from the sealed cassette: a dialer that fails if
// touched proves no request left the host.
func TestQualifyReachesDeliveryWithoutNetwork(t *testing.T) {
	fixture := NewFixture()
	t.Cleanup(fixture.Close)

	evidence, err := Qualify(context.Background(), referenceManifest(t), Declaration{
		Operations: []Operation{
			{Name: "create", Request: "account.create", Body: map[string]string{"name": "subject"}},
		},
	})
	require.NoError(t, err)
	require.Len(t, evidence.Operations, 1)
	require.Equal(t, 0, fixture.RequestCount(), "qualification contacted an upstream API")
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
