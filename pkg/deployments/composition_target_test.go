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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestCompositionTargetRequiresExplicitScopeAndRetainedIdentity(t *testing.T) {
	for _, namespace := range []string{"", "--all-namespaces", "A", "../default", strings.Repeat("a", 64)} {
		result, err := InspectLocalKubernetesTarget(t.Context(), &resources.Environment{Namespace: namespace})
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
	result, err = InspectLocalKubernetesTarget(ctx, &resources.Environment{Namespace: "explicit"})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	result, err = InspectLocalKubernetesTarget(t.Context(), &resources.Environment{Namespace: "explicit", Cluster: &resources.EnvironmentCluster{Kind: "remote"}})
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

func TestDisposableK3dCompositionTargetIdentity(t *testing.T) {
	q := requireDisposableK3d(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	initial, err := InspectLocalKubernetesTarget(ctx, q.env)
	require.NoError(t, err)
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, initial.Identity)
	require.Equal(t, q.namespace, initial.Binding.Namespace)
	require.NotEmpty(t, initial.Binding.SystemNamespaceUID)
	require.NotEmpty(t, initial.Binding.NamespaceUID)
	require.Equal(t, q.target.ClusterIdentity, initial.Binding.ClusterIdentity)
	checked, err := RecheckLocalKubernetesTarget(ctx, q.env, initial.Identity)
	require.NoError(t, err)
	require.Equal(t, initial.Binding, checked.Binding)
	workspace := t.TempDir()
	configuration, err := yaml.Marshal(&resources.Workspace{Name: "live-target", Layout: "modules", Environments: []*resources.Environment{q.env}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, resources.WorkspaceConfigurationName), configuration, 0o600))
	command := exec.CommandContext(ctx, "go", "run", "../../cmd/codefly", "composition", "--workspace", workspace,
		"inspect-local-target", q.env.Name, "--expected-identity", initial.Identity)
	command.Env = append(os.Environ(), "GOWORK=off", "CODEFLY_SILENT=true")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	var cliInspection KubernetesTargetInspection
	require.NoError(t, json.Unmarshal(output, &cliInspection), "%s", output)
	require.Equal(t, initial.Identity, cliInspection.Identity, "real CLI command must use the same live target binding")
	data, err := json.Marshal(initial)
	require.NoError(t, err)
	for _, secret := range []string{q.kubeconfig, "client-key", "client-certificate", "certificate-authority-data", "token"} {
		require.NotContains(t, string(data), secret)
	}
	runQualificationCommand(t, "kubectl", "--kubeconfig", q.kubeconfig, "annotate", "namespace", q.namespace, "example.test/observation=changed")
	checked, err = RecheckLocalKubernetesTarget(ctx, q.env, initial.Identity)
	require.NoError(t, err, "ordinary metadata updates must not change target identity")
	require.Equal(t, initial.Identity, checked.Identity)

	runQualificationCommand(t, "kubectl", "--kubeconfig", q.kubeconfig, "patch", "namespace", q.namespace, "--type=merge", "-p", `{"metadata":{"finalizers":["example.test/hold"]}}`)
	runQualificationCommand(t, "kubectl", "--kubeconfig", q.kubeconfig, "delete", "namespace", q.namespace, "--wait=false")
	checked, err = RecheckLocalKubernetesTarget(ctx, q.env, initial.Identity)
	require.ErrorContains(t, err, "active, non-deleting identity")
	require.Nil(t, checked)
	runQualificationCommand(t, "kubectl", "--kubeconfig", q.kubeconfig, "patch", "namespace", q.namespace, "--type=merge", "-p", `{"metadata":{"finalizers":[]}}`)
	runQualificationCommand(t, "kubectl", "--kubeconfig", q.kubeconfig, "wait", "--for=delete", "namespace/"+q.namespace, "--timeout=60s")
	checked, err = InspectLocalKubernetesTarget(ctx, q.env)
	require.Error(t, err, "missing namespace cannot fall back to default")
	require.Nil(t, checked)
	runQualificationCommand(t, "kubectl", "--kubeconfig", q.kubeconfig, "create", "namespace", q.namespace)
	current, err := InspectLocalKubernetesTarget(ctx, q.env)
	require.NoError(t, err)
	require.NotEqual(t, initial.Binding.NamespaceUID, current.Binding.NamespaceUID)
	require.NotEqual(t, initial.Identity, current.Identity)
	checked, err = RecheckLocalKubernetesTarget(ctx, q.env, initial.Identity)
	require.ErrorContains(t, err, "differs from the retained binding")
	require.Nil(t, checked)
}
