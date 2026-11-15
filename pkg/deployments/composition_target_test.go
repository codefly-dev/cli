package deployments

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/stretchr/testify/require"
)

func TestCompositionTargetRequiresExplicitScopeAndRetainedIdentity(t *testing.T) {
	for _, namespace := range []string{"", "--all-namespaces", "A", "../default", strings.Repeat("a", 64)} {
		result, err := InspectLocalKubernetesTarget(t.Context(), &environments.Environment{Namespace: namespace})
		require.ErrorContains(t, err, "explicit valid environment namespace")
		require.Nil(t, result)
		_, err = readActiveNamespaceUID(t.Context(), nil, nil, namespace)
		require.ErrorContains(t, err, "canonical namespace name")
	}
	result, err := InspectLocalKubernetesTarget(t.Context(), nil)
	require.Error(t, err)
	require.Nil(t, result)
	for _, identity := range []string{"", "sha256:abc", "SHA256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("A", 64)} {
		result, err = RecheckLocalKubernetesTarget(t.Context(), nil, identity)
		require.ErrorContains(t, err, "independently retained canonical")
		require.Nil(t, result)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err = InspectLocalKubernetesTarget(ctx, &environments.Environment{Namespace: "explicit"})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	result, err = InspectLocalKubernetesTarget(t.Context(), &environments.Environment{Namespace: "explicit", Cluster: &environments.EnvironmentCluster{Kind: "remote"}})
	require.ErrorContains(t, err, "exact local k3d target")
	require.Nil(t, result)
}

func TestCompositionTargetTLSRejectsUnauthenticatedRouting(t *testing.T) {
	ca := testClusterCA(t)
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{name: "valid"},
		{name: "http", change: func(cluster map[string]any) { cluster["server"] = "http://localhost:6443" }},
		{name: "credentials", change: func(cluster map[string]any) { cluster["server"] = "https://secret:private@localhost:6443" }},
		{name: "skip-tls", change: func(cluster map[string]any) { cluster["insecure-skip-tls-verify"] = true }},
		{name: "invalid-skip", change: func(cluster map[string]any) { cluster["insecure-skip-tls-verify"] = map[string]any{"bad": true} }},
		{name: "missing-ca", change: func(cluster map[string]any) { delete(cluster, "certificate-authority-data") }},
		{name: "invalid-ca", change: func(cluster map[string]any) { cluster["certificate-authority-data"] = "secret-not-a-cert" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cluster := map[string]any{"server": "https://localhost:6443", "certificate-authority-data": ca}
			if test.change != nil {
				test.change(cluster)
			}
			data, marshalErr := json.Marshal(map[string]any{"clusters": []any{map[string]any{"name": "selected", "cluster": cluster}}})
			require.NoError(t, marshalErr)
			checkErr := validateTargetTLS(data, "selected")
			if test.change == nil {
				require.NoError(t, checkErr)
			} else {
				require.Error(t, checkErr)
				require.NotContains(t, checkErr.Error(), "secret")
			}
			require.Error(t, validateTargetTLS(data, "missing"))
		})
	}
}

// testClusterCA is a throwaway certificate authority, base64 the way a
// kubeconfig carries certificate-authority-data.
func testClusterCA(t *testing.T) string {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func targetNamespaceDocument(name, uid, phase string, deleting bool) string {
	metadata := map[string]any{"name": name, "uid": uid}
	if deleting {
		metadata["deletionTimestamp"] = "2026-11-15T00:00:00Z"
	}
	encoded, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": metadata,
		"status": map[string]any{"phase": phase}})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// The target identity is an approval gate: a digest retained at approval must
// stop matching when the namespace it was taken from is replaced, and the
// inspection must refuse a deleting or missing namespace rather than fall back.
// A stub kubectl answers the namespace reads, so no cluster is started.
func TestCompositionTargetIdentityBindsTheLiveNamespace(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	const secretToken = "secret-bearer-token"
	config := map[string]any{
		"apiVersion": "v1", "current-context": "k3d-dev",
		"contexts": []any{map[string]any{"name": "k3d-dev", "context": map[string]any{"cluster": "k3d-dev", "user": "admin"}}},
		"clusters": []any{map[string]any{"name": "k3d-dev", "cluster": map[string]any{
			"server": "https://127.0.0.1:6443", "certificate-authority-data": testClusterCA(t)}}},
		"users": []any{map[string]any{"name": "admin", "user": map[string]any{"token": secretToken}}},
	}
	encoded, err := json.Marshal(config)
	require.NoError(t, err)
	harness.writeSelected(string(encoded))
	harness.writeOwned(string(encoded))
	env := harness.environment("k3d-dev")
	env.Namespace = "payments"
	harness.clusterHas("namespace", "kube-system", targetNamespaceDocument("kube-system", "system-uid", "Active", false))
	harness.clusterHas("namespace", "payments", targetNamespaceDocument("payments", "payments-uid-1", "Active", false))

	initial, err := InspectLocalKubernetesTarget(t.Context(), env)
	require.NoError(t, err)
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, initial.Identity)
	require.Equal(t, KubernetesTargetBinding{Schema: "codefly/kubernetes-target/v1", ClusterIdentity: initial.Binding.ClusterIdentity,
		SystemNamespaceUID: "system-uid", Namespace: "payments", NamespaceUID: "payments-uid-1"}, initial.Binding)
	require.NotEmpty(t, initial.Binding.ClusterIdentity)
	data, err := json.Marshal(initial)
	require.NoError(t, err)
	for _, secret := range []string{secretToken, harness.kubeconfig, "certificate-authority-data", "token"} {
		require.NotContains(t, string(data), secret)
	}

	checked, err := RecheckLocalKubernetesTarget(t.Context(), env, initial.Identity)
	require.NoError(t, err, "an unchanged namespace keeps its identity")
	require.Equal(t, initial.Identity, checked.Identity)

	harness.clusterHas("namespace", "payments", targetNamespaceDocument("payments", "payments-uid-1", "Terminating", true))
	checked, err = RecheckLocalKubernetesTarget(t.Context(), env, initial.Identity)
	require.ErrorContains(t, err, "active, non-deleting identity")
	require.Nil(t, checked)

	require.NoError(t, os.Remove(filepath.Join(harness.resourceDir, "namespace.payments.json")))
	checked, err = InspectLocalKubernetesTarget(t.Context(), env)
	require.Error(t, err, "a missing namespace cannot fall back to the context's default")
	require.Nil(t, checked)

	harness.clusterHas("namespace", "payments", targetNamespaceDocument("payments", "payments-uid-2", "Active", false))
	current, err := InspectLocalKubernetesTarget(t.Context(), env)
	require.NoError(t, err)
	require.Equal(t, "payments-uid-2", current.Binding.NamespaceUID)
	require.NotEqual(t, initial.Identity, current.Identity)
	checked, err = RecheckLocalKubernetesTarget(t.Context(), env, initial.Identity)
	require.ErrorContains(t, err, "requalification and approval are required")
	require.Nil(t, checked)
}
