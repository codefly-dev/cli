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
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	require.NoError(t, err)
	ca := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
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
