package localcluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/core/resources"
)

func certificatePEM(t *testing.T, isCA bool) ([]byte, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "k3s-server-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(der)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), hex.EncodeToString(digest[:])
}

// TestTheLocalProfileIsBoundToTheClustersAuthority: the fingerprint is the
// SHA-256 of the DER of the one CA the cluster projects — the value the host
// proves at boot — and a bundle that cannot say which CA the API server
// serves under is refused rather than guessed.
func TestTheLocalProfileIsBoundToTheClustersAuthority(t *testing.T) {
	ca, want := certificatePEM(t, true)
	got, err := ClusterCAFingerprint(ca)
	if err != nil || got != want {
		t.Fatalf("fingerprint %s (%v), want %s", got, err, want)
	}
	other, _ := certificatePEM(t, true)
	leaf, _ := certificatePEM(t, false)
	for name, bundle := range map[string][]byte{"no certificate": []byte("not pem"), "two authorities": append(append([]byte{}, ca...), other...), "not an authority": leaf} {
		if _, err := ClusterCAFingerprint(bundle); err == nil {
			t.Errorf("%s: a fingerprint was taken", name)
		}
	}
}

// TestTheVerificationPolicyIsTheHostsLocalProfile pins the document to the
// host's schema (module-saas-starter#1066): the local block with the cluster
// fingerprint, one person signer per listed developer with the exact issuer
// and address, each speaking for the environment's ownership domain.
func TestTheVerificationPolicyIsTheHostsLocalProfile(t *testing.T) {
	fingerprint := strings.Repeat("ab", 32)
	host := &environments.EnvironmentHost{Domain: "example", Developers: []environments.HostDeveloper{
		{Issuer: "https://github.com/login/oauth", Subject: "zed@example.test"},
		{Issuer: "https://accounts.google.com", Subject: "ann@example.test"},
	}}
	raw, err := VerificationPolicy(fingerprint, host)
	if err != nil {
		t.Fatal(err)
	}
	var policy map[string]any
	if err = json.Unmarshal(raw, &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy) != 2 || policy["local"].(map[string]any)["clusterCertificateAuthoritySha256"] != fingerprint {
		t.Fatalf("policy %s does not bind the cluster", raw)
	}
	signers := policy["signers"].([]any)
	first := signers[0].(map[string]any)
	if len(signers) != 2 || len(first) != 5 || first["kind"] != "person" || first["issuer"] != "https://accounts.google.com" ||
		first["subjectAlternativeName"] != "ann@example.test" || first["name"] != "ann@example.test" ||
		len(first["domains"].([]any)) != 1 || first["domains"].([]any)[0] != "example" {
		t.Fatalf("signers %s are not the listed developers as persons of the environment's domain", raw)
	}
	for name, fp := range map[string]string{"short": "abcd", "upper": strings.Repeat("AB", 32)} {
		if _, err = VerificationPolicy(fp, host); err == nil {
			t.Errorf("%s fingerprint accepted", name)
		}
	}
	if _, err = VerificationPolicy(fingerprint, &environments.EnvironmentHost{Domain: "example"}); err == nil {
		t.Error("a profile admitting nobody was written")
	}
}

// recordingKubectl answers the reads a provisioning makes and records the
// writes.
type recordingKubectl struct {
	caBundle  string
	applyOut  string
	workloads string
	calls     []string
	stdins    []string
}

func (k *recordingKubectl) run(_ context.Context, stdin string, args ...string) (string, error) {
	call := strings.Join(args, " ")
	k.calls = append(k.calls, call)
	k.stdins = append(k.stdins, stdin)
	switch {
	case strings.Contains(call, "kube-root-ca.crt"):
		return k.caBundle, nil
	case strings.HasPrefix(call, "apply"):
		return k.applyOut, nil
	case strings.Contains(call, "get deployments,statefulsets"):
		return k.workloads, nil
	}
	return "", nil
}

// TestARunProvisionsTheTrustAnchorAndReprovisionsARecreatedCluster: the run
// writes the policy and the trusted root into the verifier's namespace under
// the names the render mounts, and when the anchor changed — a recreated
// cluster's new authority — restarts exactly the workloads that mount it,
// because the host reads its anchor at boot.
func TestARunProvisionsTheTrustAnchorAndReprovisionsARecreatedCluster(t *testing.T) {
	ca, fingerprint := certificatePEM(t, true)
	env := &environments.Environment{
		Name: "local", Namespace: "platform",
		Cluster: &environments.EnvironmentCluster{Kind: environments.ClusterKindK3d, Context: "k3d-dev"},
		Host: &environments.EnvironmentHost{
			Domain: "example", Delivery: "saas/accounts/rest", TrustAnchor: "/etc/host/delivery-trust",
			Developers: []environments.HostDeveloper{{Issuer: "https://github.com/login/oauth", Subject: "dev@example.test"}},
		},
	}
	kubectl := &recordingKubectl{
		caBundle: string(ca),
		applyOut: "configmap/codefly-delivery-trust configured\nsecret/codefly-delivery-trust unchanged\n",
		workloads: "Deployment/accounts " + gitops.LocalTrustAnchorVolume + " codefly-workload-identity\n" +
			"Deployment/gateway codefly-workload-identity\n",
	}
	driver := &ClusterDriver{
		Workspace: &resources.Workspace{Name: "example"}, Environment: env, Kubectl: kubectl.run,
		TrustedRoot: func(context.Context) ([]byte, error) { return []byte(`{"mediaType":"root"}`), nil },
	}
	got, err := driver.ProvisionTrustAnchor(context.Background())
	if err != nil || got != fingerprint {
		t.Fatalf("bound to %s (%v), want %s", got, err, fingerprint)
	}
	var manifest string
	for index, call := range kubectl.calls {
		if strings.HasPrefix(call, "apply") {
			manifest = kubectl.stdins[index]
		}
	}
	for _, want := range []string{`"kind":"ConfigMap"`, `"kind":"Secret"`, `"name":"` + gitops.LocalTrustAnchorObjects + `"`, `"namespace":"platform"`, VerificationPolicyFile, TrustedRootFile, fingerprint, `{\"mediaType\":\"root\"}`} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("the trust anchor manifest lacks %s:\n%s", want, manifest)
		}
	}
	restarts := 0
	for _, call := range kubectl.calls {
		if strings.Contains(call, "rollout restart") {
			restarts++
			if !strings.HasSuffix(call, "deployment/accounts") {
				t.Fatalf("restarted %q, which does not mount the trust anchor", call)
			}
		}
	}
	if restarts != 1 {
		t.Fatalf("restarted %d workloads on a changed anchor, want the verifier alone", restarts)
	}

	unchanged := &recordingKubectl{caBundle: string(ca), applyOut: "configmap/codefly-delivery-trust unchanged\n", workloads: kubectl.workloads}
	driver.Kubectl = unchanged.run
	if _, err = driver.ProvisionTrustAnchor(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range unchanged.calls {
		if strings.Contains(call, "rollout restart") {
			t.Fatal("an unchanged anchor restarted the host")
		}
	}
}
